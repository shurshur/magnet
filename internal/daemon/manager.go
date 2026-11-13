// Package daemon implements the long-running magnet lookup service.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/magnetutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
	"github.com/shurshur/magnet/internal/store"
)

// Errors returned by Manager methods. The API maps them to HTTP status codes.
var (
	ErrNotFound      = errors.New("job not found")
	ErrNotCancelable = errors.New("job cannot be canceled")
	ErrNotRetryable  = errors.New("job cannot be retried")
	ErrNotReady      = errors.New("torrent file is not available yet")
)

// ValidationError reports an invalid magnet link in a batch.
type ValidationError struct {
	Link string
	Err  error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %v", e.Link, e.Err)
}

func (e *ValidationError) Unwrap() error { return e.Err }

// Fetcher retrieves metadata for a magnet link.
type Fetcher interface {
	Fetch(ctx context.Context, magnet string) (*metafetch.Metadata, error)
}

// Config controls the job manager.
type Config struct {
	Store   *store.Store
	Fetcher Fetcher
	// OutputDir is where .torrent files are stored, named <info-hash>.torrent.
	OutputDir string
	// Workers is the number of concurrent metadata lookups.
	Workers int
	// AttemptTimeout limits a single lookup. Zero means no limit.
	AttemptTimeout time.Duration
	// RetryDelay is the pause before a failed attempt is retried.
	RetryDelay time.Duration
	// MaxAttempts limits retries per job. Zero means unlimited.
	MaxAttempts int
	Logger      *slog.Logger
}

// Manager owns the job map, worker pool and persisted state.
type Manager struct {
	cfg       Config
	log       *slog.Logger
	store     *store.Store
	outputDir string

	mu     sync.Mutex
	jobs   map[string]*model.Job
	queue  []string
	queued map[string]struct{}
	active map[string]activeAttempt
	wake   chan struct{}

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	started   bool
	closed    bool
	startedAt time.Time
	seq       uint64
}

type activeAttempt struct {
	token  uint64
	cancel context.CancelFunc
}

// New creates a manager and loads persisted jobs. Call Start to launch workers.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("daemon: store is required")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("daemon: fetcher is required")
	}
	if cfg.OutputDir == "" {
		return nil, errors.New("daemon: output directory is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	out, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	jobs, err := cfg.Store.LoadAll()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:       cfg,
		log:       cfg.Logger,
		store:     cfg.Store,
		outputDir: out,
		jobs:      make(map[string]*model.Job, len(jobs)),
		queued:    make(map[string]struct{}),
		active:    make(map[string]activeAttempt),
		wake:      make(chan struct{}, 1),
	}
	for i := range jobs {
		j := jobs[i]
		m.jobs[j.ID] = &j
	}
	return m, nil
}

// Start resumes persisted jobs and launches the worker pool.
func (m *Manager) Start(parent context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("daemon: manager already started")
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	m.started = true
	m.startedAt = time.Now()

	now := time.Now()
	var changed []model.Job
	type delayed struct {
		id string
		d  time.Duration
	}
	var later []delayed

	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return m.jobs[ids[i]].CreatedAt.Before(m.jobs[ids[j]].CreatedAt)
	})

	for _, id := range ids {
		j := m.jobs[id]
		switch j.State {
		case model.StateSearching:
			// The previous daemon process stopped mid-lookup.
			j.State = model.StateQueued
			j.StartedAt = nil
			j.NextAttemptAt = nil
			j.Error = ""
			j.UpdatedAt = now
			changed = append(changed, *j)
			m.enqueueLocked(id)
		case model.StateQueued:
			if j.NextAttemptAt != nil && j.NextAttemptAt.After(now) {
				later = append(later, delayed{id: id, d: time.Until(*j.NextAttemptAt)})
			} else {
				j.NextAttemptAt = nil
				m.enqueueLocked(id)
			}
		case model.StateFound:
			path := j.Path
			if path == "" {
				path = filepath.Join(m.outputDir, id+".torrent")
				j.Path = path
			}
			if st, err := os.Stat(path); err != nil || st.IsDir() {
				j.State = model.StateQueued
				j.Path = ""
				j.StartedAt = nil
				j.FinishedAt = nil
				j.NextAttemptAt = nil
				j.Error = "saved torrent file is missing; queued for lookup again"
				j.UpdatedAt = now
				changed = append(changed, *j)
				m.enqueueLocked(id)
			}
		}
	}
	if len(changed) > 0 {
		if err := m.store.Put(changed...); err != nil {
			m.log.Error("failed to persist resumed job state", "error", err)
		}
	}
	queuedCount := len(m.queued)
	jobCount := len(m.jobs)
	m.mu.Unlock()

	for _, d := range later {
		m.schedule(d.id, d.d)
	}

	for i := 0; i < m.cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	m.log.Info("job manager started",
		"jobs", jobCount,
		"queued", queuedCount,
		"workers", m.cfg.Workers,
		"attempt_timeout", m.cfg.AttemptTimeout.String(),
		"retry_delay", m.cfg.RetryDelay.String(),
		"max_attempts", m.cfg.MaxAttempts,
	)
	return nil
}

// Close stops workers. Jobs that were searching are re-queued for the next start.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var changed []model.Job
	for _, j := range m.jobs {
		if j.State == model.StateSearching {
			j.State = model.StateQueued
			j.StartedAt = nil
			j.NextAttemptAt = nil
			j.UpdatedAt = now
			changed = append(changed, *j)
		}
	}
	if len(changed) > 0 {
		if err := m.store.Put(changed...); err != nil {
			m.log.Error("failed to persist jobs during shutdown", "error", err)
		}
	}
}

// AddBatch validates and adds a batch of magnet links. Invalid links reject the
// whole batch, so a client never gets a partially applied request. Duplicate
// links (by info hash) return the existing job.
func (m *Manager) AddBatch(links []string) (jobs []model.Job, created int, err error) {
	type parsed struct {
		link string
		id   string
	}
	inputs := make([]parsed, 0, len(links))
	for _, link := range links {
		id, canonical, parseErr := magnetutil.Parse(link)
		if parseErr != nil {
			return nil, 0, &ValidationError{Link: link, Err: parseErr}
		}
		inputs = append(inputs, parsed{link: canonical, id: id})
	}
	if len(inputs) == 0 {
		return nil, 0, &ValidationError{Err: errors.New("no magnet links supplied")}
	}

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make([]model.Job, len(inputs))
	newJobs := make([]model.Job, 0, len(inputs))
	newByID := make(map[string]model.Job)
	createdIDs := make(map[string]struct{})

	for i, in := range inputs {
		if j, ok := m.jobs[in.id]; ok {
			results[i] = *j
			continue
		}
		if j, ok := newByID[in.id]; ok {
			results[i] = j
			continue
		}

		var j model.Job
		path := filepath.Join(m.outputDir, in.id+".torrent")
		if existing, ok := m.jobFromExistingFile(in.link, in.id, path); ok {
			j = existing
		} else {
			j = model.Job{
				ID:        in.id,
				Magnet:    in.link,
				InfoHash:  in.id,
				State:     model.StateQueued,
				CreatedAt: now,
				UpdatedAt: now,
			}
		}
		newByID[in.id] = j
		newJobs = append(newJobs, j)
		createdIDs[in.id] = struct{}{}
		results[i] = j
	}

	if len(newJobs) > 0 {
		if err := m.store.Put(newJobs...); err != nil {
			return nil, 0, err
		}
		for id, j := range newByID {
			jc := j
			m.jobs[id] = &jc
			if jc.State == model.StateQueued {
				m.enqueueLocked(id)
			}
			m.log.Info("job added",
				"id", id,
				"state", jc.State,
				"name", jc.Name,
			)
		}
		m.signalLocked()
	}
	return results, len(createdIDs), nil
}

// Add is a convenience wrapper for a single magnet link.
func (m *Manager) Add(link string) (model.Job, bool, error) {
	jobs, created, err := m.AddBatch([]string{link})
	if err != nil {
		return model.Job{}, false, err
	}
	return jobs[0], created == 1, nil
}

func (m *Manager) jobFromExistingFile(link, id, path string) (model.Job, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return model.Job{}, false
	}
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		m.log.Warn("ignoring unreadable existing torrent file", "path", path, "error", err)
		return model.Job{}, false
	}
	if mi.HashInfoBytes().HexString() != id {
		m.log.Warn("existing torrent file has a different info hash", "path", path)
		return model.Job{}, false
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		m.log.Warn("ignoring existing torrent file with invalid info", "path", path, "error", err)
		return model.Job{}, false
	}
	now := time.Now()
	return model.Job{
		ID:         id,
		Magnet:     link,
		InfoHash:   id,
		Name:       info.Name,
		State:      model.StateFound,
		Path:       path,
		Length:     info.TotalLength(),
		Files:      len(info.UpvertedFiles()),
		CreatedAt:  now,
		UpdatedAt:  now,
		FinishedAt: &now,
	}, true
}

// Get returns a job by ID.
func (m *Manager) Get(id string) (model.Job, error) {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return model.Job{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[norm]
	if !ok {
		return model.Job{}, ErrNotFound
	}
	return *j, nil
}

// ListOptions filter and paginate job listings.
type ListOptions struct {
	State  model.State
	Limit  int
	Offset int
}

// List returns jobs sorted by creation time, newest first.
func (m *Manager) List(opts ListOptions) ([]model.Job, int, error) {
	if opts.State != "" {
		switch opts.State {
		case model.StateQueued, model.StateSearching, model.StateFound, model.StateFailed, model.StateCanceled:
		default:
			return nil, 0, fmt.Errorf("unknown state %q", opts.State)
		}
	}
	m.mu.Lock()
	jobs := make([]model.Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if opts.State != "" && j.State != opts.State {
			continue
		}
		jobs = append(jobs, *j)
	}
	m.mu.Unlock()

	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].CreatedAt.Equal(jobs[k].CreatedAt) {
			return jobs[i].ID < jobs[k].ID
		}
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	total := len(jobs)
	if opts.Offset >= total {
		return []model.Job{}, total, nil
	}
	if opts.Offset > 0 {
		jobs = jobs[opts.Offset:]
	}
	if opts.Limit > 0 && len(jobs) > opts.Limit {
		jobs = jobs[:opts.Limit]
	}
	return jobs, total, nil
}

// Cancel stops an active lookup or drops a queued job. The record is kept.
func (m *Manager) Cancel(id string) (model.Job, error) {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return model.Job{}, ErrNotFound
	}
	m.mu.Lock()
	j, ok := m.jobs[norm]
	if !ok {
		m.mu.Unlock()
		return model.Job{}, ErrNotFound
	}
	switch j.State {
	case model.StateFound:
		m.mu.Unlock()
		return model.Job{}, ErrNotCancelable
	case model.StateCanceled:
		cp := *j
		m.mu.Unlock()
		return cp, nil
	}
	now := time.Now()
	j.State = model.StateCanceled
	j.Error = ""
	j.NextAttemptAt = nil
	j.StartedAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	delete(m.queued, norm)
	act, isActive := m.active[norm]
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist canceled job", "id", norm, "error", err)
	}
	cp := *j
	m.mu.Unlock()

	if isActive {
		act.cancel()
	}
	m.log.Info("job canceled", "id", norm)
	return cp, nil
}

// Retry re-queues a failed or canceled job.
func (m *Manager) Retry(id string) (model.Job, error) {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return model.Job{}, ErrNotFound
	}
	m.mu.Lock()
	j, ok := m.jobs[norm]
	if !ok {
		m.mu.Unlock()
		return model.Job{}, ErrNotFound
	}
	switch j.State {
	case model.StateQueued, model.StateSearching, model.StateFound:
		m.mu.Unlock()
		return model.Job{}, ErrNotRetryable
	}
	now := time.Now()
	j.State = model.StateQueued
	j.Error = ""
	j.Attempts = 0
	j.StartedAt = nil
	j.FinishedAt = nil
	j.NextAttemptAt = nil
	j.UpdatedAt = now
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist retried job", "id", norm, "error", err)
	}
	m.enqueueLocked(norm)
	cp := *j
	m.mu.Unlock()
	m.signal()
	m.log.Info("job queued for retry", "id", norm)
	return cp, nil
}

// Delete cancels (if needed) and removes a job. With purge it also deletes the
// saved .torrent file.
func (m *Manager) Delete(id string, purge bool) error {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return ErrNotFound
	}
	m.mu.Lock()
	j, ok := m.jobs[norm]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	path := j.Path
	act, isActive := m.active[norm]
	delete(m.jobs, norm)
	delete(m.queued, norm)
	if err := m.store.Delete(norm); err != nil {
		m.log.Error("failed to delete job from store", "id", norm, "error", err)
	}
	m.mu.Unlock()

	if isActive {
		act.cancel()
	}
	if purge && path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			m.log.Warn("failed to remove torrent file", "path", path, "error", err)
		}
	}
	m.log.Info("job removed", "id", norm, "purge", purge)
	return nil
}

// TorrentFile returns the path and suggested file name for a completed job.
func (m *Manager) TorrentFile(id string) (path, name string, err error) {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return "", "", ErrNotFound
	}
	m.mu.Lock()
	j, ok := m.jobs[norm]
	if !ok {
		m.mu.Unlock()
		return "", "", ErrNotFound
	}
	if j.State != model.StateFound || j.Path == "" {
		m.mu.Unlock()
		return "", "", ErrNotReady
	}
	path, name = j.Path, j.Name
	m.mu.Unlock()

	st, statErr := os.Stat(path)
	if statErr != nil || st.IsDir() {
		return "", "", ErrNotReady
	}
	return path, name, nil
}

// Stats summarizes the job states.
type Stats struct {
	Total     int `json:"total"`
	Queued    int `json:"queued"`
	Searching int `json:"searching"`
	Found     int `json:"found"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	Active    int `json:"active"`
}

// Stats returns current job counters.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Stats{Total: len(m.jobs), Active: len(m.active)}
	for _, j := range m.jobs {
		switch j.State {
		case model.StateQueued:
			s.Queued++
		case model.StateSearching:
			s.Searching++
		case model.StateFound:
			s.Found++
		case model.StateFailed:
			s.Failed++
		case model.StateCanceled:
			s.Canceled++
		}
	}
	return s
}

// Uptime returns how long the manager has been running.
func (m *Manager) Uptime() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startedAt.IsZero() {
		return 0
	}
	return time.Since(m.startedAt)
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		if m.ctx.Err() != nil {
			return
		}
		att, ok := m.claim()
		if ok {
			m.runAttempt(att)
			continue
		}
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		}
	}
}

type attempt struct {
	job   model.Job
	token uint64
}

func (m *Manager) claim() (attempt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.queue) > 0 {
		id := m.queue[0]
		m.queue = m.queue[1:]
		if _, ok := m.queued[id]; !ok {
			continue
		}
		delete(m.queued, id)
		j, ok := m.jobs[id]
		if !ok || j.State != model.StateQueued {
			continue
		}
		now := time.Now()
		j.State = model.StateSearching
		j.Attempts++
		j.StartedAt = &now
		j.UpdatedAt = now
		j.NextAttemptAt = nil
		j.Error = ""
		if err := m.store.Put(*j); err != nil {
			m.log.Error("failed to persist started job", "id", id, "error", err)
		}
		m.seq++
		return attempt{job: *j, token: m.seq}, true
	}
	return attempt{}, false
}

func (m *Manager) runAttempt(att attempt) {
	ctx := m.ctx
	cancel := context.CancelFunc(func() {})
	if m.cfg.AttemptTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, m.cfg.AttemptTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	m.mu.Lock()
	j, ok := m.jobs[att.job.ID]
	if !ok || j.State != model.StateSearching {
		m.mu.Unlock()
		cancel()
		return
	}
	m.active[att.job.ID] = activeAttempt{token: att.token, cancel: cancel}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		if cur, ok := m.active[att.job.ID]; ok && cur.token == att.token {
			delete(m.active, att.job.ID)
		}
		m.mu.Unlock()
		cancel()
	}()

	m.log.Info("metadata search started",
		"id", att.job.ID,
		"attempt", att.job.Attempts,
		"name", att.job.Name,
	)

	md, err := m.cfg.Fetcher.Fetch(ctx, att.job.Magnet)
	if err == nil && md == nil {
		err = errors.New("metadata fetcher returned nil result")
	}
	if err == nil && md.InfoHash != "" && md.InfoHash != att.job.ID {
		err = fmt.Errorf("metadata info hash mismatch: got %s, want %s", md.InfoHash, att.job.ID)
	}
	if err == nil {
		m.finishFound(att, md)
		return
	}
	m.finishFailed(att, err)
}

func (m *Manager) finishFound(att attempt, md *metafetch.Metadata) {
	path := filepath.Join(m.outputDir, att.job.ID+".torrent")

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isCurrentAttemptLocked(att) {
		return
	}
	j := m.jobs[att.job.ID]
	if err := fsutil.AtomicWriteFile(path, md.Torrent, 0o644); err != nil {
		m.log.Error("failed to save torrent file", "id", att.job.ID, "error", err)
		m.scheduleRetryLocked(att, err)
		return
	}
	now := time.Now()
	j.State = model.StateFound
	j.Name = md.Name
	j.Path = path
	j.Length = md.Length
	j.Files = md.Files
	j.Error = ""
	j.NextAttemptAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist completed job", "id", att.job.ID, "error", err)
	}
	m.log.Info("metadata found",
		"id", att.job.ID,
		"name", md.Name,
		"length", md.Length,
		"files", md.Files,
		"path", path,
		"attempt", j.Attempts,
	)
}

func (m *Manager) finishFailed(att attempt, fetchErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isCurrentAttemptLocked(att) {
		// Canceled, removed or superseded while the lookup was in flight.
		return
	}
	j := m.jobs[att.job.ID]
	if m.ctx.Err() != nil {
		// Daemon shutdown: leave the job queued for the next start.
		j.State = model.StateQueued
		j.StartedAt = nil
		j.NextAttemptAt = nil
		j.UpdatedAt = time.Now()
		if err := m.store.Put(*j); err != nil {
			m.log.Error("failed to persist job during shutdown", "id", att.job.ID, "error", err)
		}
		return
	}
	m.scheduleRetryLocked(att, fetchErr)
}

// isCurrentAttemptLocked reports whether att still owns the active slot for
// its job. m.mu must be held.
func (m *Manager) isCurrentAttemptLocked(att attempt) bool {
	cur, ok := m.active[att.job.ID]
	if !ok || cur.token != att.token {
		return false
	}
	j, ok := m.jobs[att.job.ID]
	return ok && j.State == model.StateSearching
}

// scheduleRetryLocked applies the retry policy. m.mu must be held.
func (m *Manager) scheduleRetryLocked(att attempt, fetchErr error) {
	j := m.jobs[att.job.ID]
	if j == nil {
		return
	}
	now := time.Now()
	msg := fetchErr.Error()
	if errors.Is(fetchErr, context.DeadlineExceeded) && m.ctx.Err() == nil {
		msg = fmt.Sprintf("metadata search timed out after %s", m.cfg.AttemptTimeout)
	}
	canRetry := m.cfg.MaxAttempts <= 0 || j.Attempts < m.cfg.MaxAttempts
	if canRetry {
		next := now.Add(m.cfg.RetryDelay)
		j.State = model.StateQueued
		j.Error = msg
		j.StartedAt = nil
		j.NextAttemptAt = &next
		j.UpdatedAt = now
		if err := m.store.Put(*j); err != nil {
			m.log.Error("failed to persist retry state", "id", j.ID, "error", err)
		}
		m.log.Warn("metadata search failed; will retry",
			"id", j.ID,
			"attempt", j.Attempts,
			"retry_in", m.cfg.RetryDelay.String(),
			"error", msg,
		)
		m.schedule(j.ID, m.cfg.RetryDelay)
		return
	}

	j.State = model.StateFailed
	j.Error = msg
	j.StartedAt = nil
	j.NextAttemptAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist failed job", "id", j.ID, "error", err)
	}
	m.log.Error("metadata search failed", "id", j.ID, "attempts", j.Attempts, "error", msg)
}

func (m *Manager) enqueueLocked(id string) {
	j, ok := m.jobs[id]
	if !ok || j.State != model.StateQueued {
		return
	}
	if _, ok := m.queued[id]; ok {
		return
	}
	m.queued[id] = struct{}{}
	m.queue = append(m.queue, id)
}

func (m *Manager) enqueue(id string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.enqueueLocked(id)
	queued := false
	if _, ok := m.queued[id]; ok {
		queued = true
	}
	m.mu.Unlock()
	if queued {
		m.signal()
	}
}

func (m *Manager) schedule(id string, d time.Duration) {
	if d <= 0 {
		// Avoid re-entering the manager lock when called from a locked section.
		go m.enqueue(id)
		return
	}
	time.AfterFunc(d, func() {
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if !closed {
			m.enqueue(id)
		}
	})
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) signalLocked() {
	m.signal()
}
