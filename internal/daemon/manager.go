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
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

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

	mu          sync.Mutex
	jobs        map[string]*model.Job
	aliases     map[string]string
	filesByHash map[string]fileInfo
	queue       []string
	queued      map[string]struct{}
	active      map[string]activeAttempt
	wake        chan struct{}

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	started   bool
	closed    bool
	startedAt time.Time
	seq       uint64
}

type fileInfo struct {
	Path       string
	InfoHashV1 string
	InfoHashV2 string
	Version    string
	Name       string
	Length     int64
	Files      int
	Complete   bool
	Warning    string
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
		cfg:         cfg,
		log:         cfg.Logger,
		store:       cfg.Store,
		outputDir:   out,
		jobs:        make(map[string]*model.Job, len(jobs)),
		aliases:     make(map[string]string, len(jobs)*2),
		filesByHash: make(map[string]fileInfo),
		queued:      make(map[string]struct{}),
		active:      make(map[string]activeAttempt),
		wake:        make(chan struct{}, 1),
	}
	for i := range jobs {
		j := jobs[i]
		if j.InfoHash == "" {
			j.InfoHash = j.ID
		}
		if len(j.Magnets) == 0 && j.Magnet != "" {
			j.Magnets = []string{j.Magnet}
		}
		m.jobs[j.ID] = &j
		m.addJobAliasesLocked(&j)
	}
	m.scanOutputDirLocked()
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
		if j.Downloadable() {
			if !m.enrichJobFromFileLocked(j, now) {
				// The saved file is gone; queue the job again.
				j.State = model.StateQueued
				j.Path = ""
				j.StartedAt = nil
				j.FinishedAt = nil
				j.NextAttemptAt = nil
				j.Error = "saved torrent file is missing; queued for lookup again"
				j.Warning = ""
				j.UpdatedAt = now
				changed = append(changed, *j)
				m.enqueueLocked(id)
				continue
			}
			changed = append(changed, *j)
		}
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
		}
	}
	if len(changed) > 0 {
		if err := m.store.Put(changed...); err != nil {
			m.log.Error("failed to persist resumed job state", "error", err)
		}
	}
	for _, id := range ids {
		if j, ok := m.jobs[id]; ok {
			m.mergeClusterLocked(j)
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
// links (by v1/v2 info hash) return the existing job.
func (m *Manager) AddBatch(links []string) (jobs []model.Job, created int, err error) {
	inputs := make([]magnetutil.Parsed, 0, len(links))
	for _, link := range links {
		parsed, parseErr := magnetutil.Parse(link)
		if parseErr != nil {
			return nil, 0, &ValidationError{Link: link, Err: parseErr}
		}
		inputs = append(inputs, parsed)
	}
	if len(inputs) == 0 {
		return nil, 0, &ValidationError{Err: errors.New("no magnet links supplied")}
	}

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make([]model.Job, len(inputs))
	newJobs := make(map[string]*model.Job)
	changed := make(map[string]model.Job)
	localAliases := make(map[string]string)
	createdIDs := make(map[string]struct{})

	for i, in := range inputs {
		hashes := in.Hashes()
		var j *model.Job
		for _, hash := range hashes {
			if id, ok := localAliases[hash]; ok {
				j = newJobs[id]
				break
			}
			if id, ok := m.aliases[hash]; ok {
				j = m.jobs[id]
				break
			}
		}
		if j == nil {
			if file, ok := m.findFileLocked(hashes); ok {
				createdJob := m.jobFromFile(in, file)
				newJobs[createdJob.ID] = &createdJob
				createdIDs[createdJob.ID] = struct{}{}
				for _, alias := range append([]string{createdJob.ID, file.InfoHashV1, file.InfoHashV2}, hashes...) {
					if alias != "" {
						localAliases[alias] = createdJob.ID
					}
				}
				j = &createdJob
			}
		}
		if j == nil {
			createdJob := model.Job{
				ID:         in.PrimaryID,
				Magnet:     in.Link,
				Magnets:    []string{in.Link},
				InfoHash:   in.PrimaryID,
				InfoHashV1: in.InfoHashV1,
				InfoHashV2: in.InfoHashV2,
				Version:    in.Version,
				State:      model.StateQueued,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			newJobs[createdJob.ID] = &createdJob
			createdIDs[createdJob.ID] = struct{}{}
			for _, alias := range append([]string{createdJob.ID, in.InfoHashV1, in.InfoHashV2}, hashes...) {
				if alias != "" {
					localAliases[alias] = createdJob.ID
				}
			}
			j = &createdJob
		} else if _, isNew := newJobs[j.ID]; isNew {
			m.rememberMagnetLocked(j, in.Link)
		} else {
			if m.rememberMagnetLocked(j, in.Link) {
				changed[j.ID] = *j
			}
		}
		results[i] = *j
	}

	toStore := make([]model.Job, 0, len(newJobs)+len(changed))
	for _, j := range newJobs {
		toStore = append(toStore, *j)
	}
	for _, j := range changed {
		toStore = append(toStore, j)
	}
	if len(toStore) > 0 {
		if err := m.store.Put(toStore...); err != nil {
			return nil, 0, err
		}
	}
	for id, j := range newJobs {
		m.jobs[id] = j
		m.addJobAliasesLocked(j)
		if j.State == model.StateQueued {
			m.enqueueLocked(id)
		}
		m.log.Info("job added",
			"id", id,
			"state", j.State,
			"name", j.Name,
			"version", j.Version,
		)
	}
	if len(newJobs) > 0 {
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

func (m *Manager) jobFromFile(in magnetutil.Parsed, file fileInfo) model.Job {
	now := time.Now()
	state := model.StateFound
	if !file.Complete {
		state = model.StateIncomplete
	}
	return model.Job{
		ID:         in.PrimaryID,
		Magnet:     in.Link,
		Magnets:    []string{in.Link},
		InfoHash:   in.PrimaryID,
		InfoHashV1: file.InfoHashV1,
		InfoHashV2: file.InfoHashV2,
		Version:    file.Version,
		Name:       file.Name,
		State:      state,
		Path:       file.Path,
		Length:     file.Length,
		Files:      file.Files,
		Warning:    file.Warning,
		CreatedAt:  now,
		UpdatedAt:  now,
		FinishedAt: &now,
	}
}

func (m *Manager) rememberMagnetLocked(j *model.Job, link string) bool {
	if link == "" {
		return false
	}
	if j.Magnet == "" {
		j.Magnet = link
	}
	for _, existing := range j.Magnets {
		if existing == link {
			return false
		}
	}
	j.Magnets = append(j.Magnets, link)
	j.UpdatedAt = time.Now()
	return true
}

func (m *Manager) addAliasLocked(alias, id string) {
	norm, err := magnetutil.NormalizeID(alias)
	if err != nil || id == "" {
		return
	}
	if existing, ok := m.aliases[norm]; !ok || existing == id {
		m.aliases[norm] = id
	}
}

func (m *Manager) addJobAliasesLocked(j *model.Job) {
	m.addAliasLocked(j.ID, j.ID)
	m.addAliasLocked(j.InfoHashV1, j.ID)
	m.addAliasLocked(j.InfoHashV2, j.ID)
	for _, alias := range j.Aliases {
		m.addAliasLocked(alias, j.ID)
	}
	if base := strings.TrimSuffix(filepath.Base(j.Path), ".torrent"); base != "" {
		m.addAliasLocked(base, j.ID)
	}
}

func (m *Manager) resolveIDLocked(id string) (string, bool) {
	norm, err := magnetutil.NormalizeID(id)
	if err != nil {
		return "", false
	}
	if canonical, ok := m.aliases[norm]; ok {
		if _, exists := m.jobs[canonical]; exists {
			return canonical, true
		}
	}
	if _, ok := m.jobs[norm]; ok {
		return norm, true
	}
	return "", false
}

func jobHashes(j *model.Job) []string {
	seen := make(map[string]struct{})
	var hashes []string
	add := func(hash string) {
		if hash == "" {
			return
		}
		if _, ok := seen[hash]; ok {
			return
		}
		seen[hash] = struct{}{}
		hashes = append(hashes, hash)
	}
	add(j.ID)
	add(j.InfoHashV1)
	add(j.InfoHashV2)
	for _, alias := range j.Aliases {
		add(alias)
	}
	return hashes
}

func (m *Manager) scanOutputDirLocked() {
	matches, _ := filepath.Glob(filepath.Join(m.outputDir, "*.torrent"))
	for _, path := range matches {
		m.indexFileLocked(path)
	}
}

func (m *Manager) findFileLocked(hashes []string) (fileInfo, bool) {
	var fallback fileInfo
	found := false
	for _, hash := range hashes {
		norm, err := magnetutil.NormalizeID(hash)
		if err != nil {
			continue
		}
		file, ok := m.filesByHash[norm]
		if !ok {
			continue
		}
		if file.Complete {
			return file, true
		}
		if !found {
			fallback = file
			found = true
		}
	}
	return fallback, found
}

func (m *Manager) indexFileLocked(path string) (fileInfo, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		m.unindexPathLocked(path)
		return fileInfo{}, false
	}
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		m.log.Warn("ignoring unreadable existing torrent file", "path", path, "error", err)
		m.unindexPathLocked(path)
		return fileInfo{}, false
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		m.log.Warn("ignoring existing torrent file with invalid info", "path", path, "error", err)
		m.unindexPathLocked(path)
		return fileInfo{}, false
	}
	v1, v2 := torrentHashes(mi, &info)
	if v1 == "" && v2 == "" {
		return fileInfo{}, false
	}
	file := fileInfo{
		Path:       path,
		InfoHashV1: v1,
		InfoHashV2: v2,
		Version:    torrentVersion(&info),
		Name:       info.BestName(),
		Length:     info.TotalLength(),
		Files:      len(info.UpvertedFiles()),
		Complete:   true,
	}
	if info.HasV2() {
		if err := metainfo.ValidatePieceLayers(mi.PieceLayers, &info.FileTree, info.PieceLength); err != nil {
			file.Complete = false
			file.Warning = "piece layers are incomplete: " + err.Error()
		}
	}
	m.unindexPathLocked(path)
	if v1 != "" {
		m.putFileIndexLocked(v1, file)
	}
	if v2 != "" {
		m.putFileIndexLocked(v2, file)
	}
	return file, true
}

func (m *Manager) putFileIndexLocked(hash string, file fileInfo) {
	if existing, ok := m.filesByHash[hash]; ok && existing.Path != file.Path {
		if existing.Complete || !file.Complete {
			return
		}
	}
	m.filesByHash[hash] = file
}

func (m *Manager) unindexPathLocked(path string) {
	for hash, file := range m.filesByHash {
		if file.Path == path {
			delete(m.filesByHash, hash)
		}
	}
}

func (m *Manager) enrichJobFromFileLocked(j *model.Job, now time.Time) bool {
	path := j.Path
	if path == "" {
		path = filepath.Join(m.outputDir, j.ID+".torrent")
	}
	file, ok := m.indexFileLocked(path)
	if !ok {
		return false
	}
	j.Path = path
	j.InfoHash = j.ID
	j.InfoHashV1 = file.InfoHashV1
	j.InfoHashV2 = file.InfoHashV2
	j.Version = file.Version
	j.Name = file.Name
	j.Length = file.Length
	j.Files = file.Files
	j.Warning = file.Warning
	if file.Complete {
		j.State = model.StateFound
		j.Warning = ""
	} else {
		j.State = model.StateIncomplete
	}
	j.UpdatedAt = now
	m.addJobAliasesLocked(j)
	return true
}

func torrentHashes(mi *metainfo.MetaInfo, info *metainfo.Info) (v1, v2 string) {
	if info.HasV1() {
		v1 = mi.HashInfoBytes().HexString()
	}
	if info.HasV2() {
		h := infohash_v2.HashBytes(mi.InfoBytes)
		v2 = h.HexString()
	}
	return
}

func torrentVersion(info *metainfo.Info) string {
	switch {
	case info.HasV1() && info.HasV2():
		return model.VersionHybrid
	case info.HasV2():
		return model.VersionV2
	default:
		return model.VersionV1
	}
}

func (m *Manager) jobRankLocked(j *model.Job) int {
	if j.Downloadable() && j.Path != "" {
		if st, err := os.Stat(j.Path); err == nil && !st.IsDir() {
			switch j.State {
			case model.StateFound:
				return 4
			case model.StateIncomplete:
				return 3
			}
		}
	}
	switch j.State {
	case model.StateSearching:
		return 2
	case model.StateQueued:
		return 1
	default:
		return 0
	}
}

func (m *Manager) mergeClusterLocked(current *model.Job) *model.Job {
	candidates := map[string]*model.Job{current.ID: current}
	for _, hash := range jobHashes(current) {
		if id, ok := m.aliases[hash]; ok {
			if j, exists := m.jobs[id]; exists {
				candidates[j.ID] = j
			}
		}
	}
	if id, ok := m.aliases[current.ID]; ok {
		if j, exists := m.jobs[id]; exists {
			candidates[j.ID] = j
		}
	}
	if len(candidates) == 1 {
		m.rebuildAliasesLocked(current)
		if err := m.store.Put(*current); err != nil {
			m.log.Error("failed to persist completed job", "id", current.ID, "error", err)
		}
		return current
	}

	list := make([]*model.Job, 0, len(candidates))
	for _, j := range candidates {
		list = append(list, j)
	}
	sort.Slice(list, func(i, k int) bool {
		ri, rk := m.jobRankLocked(list[i]), m.jobRankLocked(list[k])
		if ri != rk {
			return ri > rk
		}
		if !list[i].CreatedAt.Equal(list[k].CreatedAt) {
			return list[i].CreatedAt.Before(list[k].CreatedAt)
		}
		return list[i].ID < list[k].ID
	})
	canonical := list[0]
	for _, other := range list[1:] {
		m.mergeJobLocked(canonical, other)
	}
	m.rebuildAliasesLocked(canonical)
	if err := m.store.Put(*canonical); err != nil {
		m.log.Error("failed to persist merged job", "id", canonical.ID, "error", err)
	}
	if canonical.Path != "" {
		m.indexFileLocked(canonical.Path)
	}
	m.log.Info("jobs merged",
		"id", canonical.ID,
		"state", canonical.State,
		"aliases", len(canonical.Aliases),
		"magnets", len(canonical.Magnets),
	)
	return canonical
}

func (m *Manager) mergeJobLocked(canonical, other *model.Job) {
	if other == nil || other.ID == canonical.ID {
		return
	}
	if canonical.Magnet == "" {
		canonical.Magnet = other.Magnet
	}
	for _, magnet := range other.Magnets {
		m.rememberMagnetLocked(canonical, magnet)
	}

	if canonical.InfoHashV1 == "" {
		canonical.InfoHashV1 = other.InfoHashV1
	}
	if canonical.InfoHashV2 == "" {
		canonical.InfoHashV2 = other.InfoHashV2
	}
	if canonical.InfoHashV2 != "" {
		for _, magnet := range canonical.Magnets {
			if strings.Contains(magnet, "urn:btmh:") {
				canonical.Magnet = magnet
				break
			}
		}
	}
	if canonical.Version == "" {
		canonical.Version = other.Version
	}
	if canonical.Name == "" {
		canonical.Name = other.Name
	}
	if canonical.Length == 0 {
		canonical.Length = other.Length
	}
	if canonical.Files == 0 {
		canonical.Files = other.Files
	}
	if other.State == model.StateFound && canonical.State != model.StateFound && other.Path != "" {
		canonical.State = other.State
		canonical.Path = other.Path
		canonical.Warning = ""
	}
	if canonical.State == model.StateIncomplete && canonical.Warning == "" {
		canonical.Warning = other.Warning
	}
	if canonical.State == model.StateFound {
		canonical.Warning = ""
	}
	if other.CreatedAt.Before(canonical.CreatedAt) {
		canonical.CreatedAt = other.CreatedAt
	}
	if other.Attempts > canonical.Attempts {
		canonical.Attempts = other.Attempts
	}
	if canonical.FinishedAt == nil {
		canonical.FinishedAt = other.FinishedAt
	}
	canonical.Aliases = append(canonical.Aliases, other.ID)
	canonical.Aliases = append(canonical.Aliases, other.Aliases...)
	if other.InfoHashV1 != "" {
		canonical.Aliases = append(canonical.Aliases, other.InfoHashV1)
	}
	if other.InfoHashV2 != "" {
		canonical.Aliases = append(canonical.Aliases, other.InfoHashV2)
	}
	canonical.Aliases = uniqueStrings(canonical.Aliases)
	canonical.InfoHash = canonical.ID
	canonical.UpdatedAt = time.Now()

	delete(m.jobs, other.ID)
	delete(m.queued, other.ID)
	if act, ok := m.active[other.ID]; ok {
		act.cancel()
		delete(m.active, other.ID)
	}
	if err := m.store.Delete(other.ID); err != nil {
		m.log.Error("failed to delete merged job", "id", other.ID, "error", err)
	}
	if other.Path != "" && other.Path != canonical.Path {
		m.unindexPathLocked(other.Path)
		if m.isOutputPath(other.Path) {
			if err := os.Remove(other.Path); err != nil && !os.IsNotExist(err) {
				m.log.Warn("failed to remove duplicate torrent file", "path", other.Path, "error", err)
			}
		}
	}
	for alias, id := range m.aliases {
		if id == other.ID {
			m.aliases[alias] = canonical.ID
		}
	}
}

func (m *Manager) rebuildAliasesLocked(j *model.Job) {
	for _, hash := range jobHashes(j) {
		norm, err := magnetutil.NormalizeID(hash)
		if err == nil {
			m.aliases[norm] = j.ID
		}
	}
	m.addJobAliasesLocked(j)
}

func (m *Manager) isOutputPath(path string) bool {
	rel, err := filepath.Rel(m.outputDir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// Get returns a job by ID, resolving merged aliases.
func (m *Manager) Get(id string) (model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	canonical, ok := m.resolveIDLocked(id)
	if !ok {
		return model.Job{}, ErrNotFound
	}
	return *m.jobs[canonical], nil
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
		case model.StateQueued, model.StateSearching, model.StateFound, model.StateIncomplete,
			model.StateFailed, model.StateCanceled:
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
	m.mu.Lock()
	canonical, ok := m.resolveIDLocked(id)
	if !ok {
		m.mu.Unlock()
		return model.Job{}, ErrNotFound
	}
	j := m.jobs[canonical]
	switch j.State {
	case model.StateFound, model.StateIncomplete:
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
	j.Warning = ""
	j.NextAttemptAt = nil
	j.StartedAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	delete(m.queued, canonical)
	act, isActive := m.active[canonical]
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist canceled job", "id", canonical, "error", err)
	}
	cp := *j
	m.mu.Unlock()

	if isActive {
		act.cancel()
	}
	m.log.Info("job canceled", "id", canonical)
	return cp, nil
}

// Retry re-queues a failed, canceled or incomplete job.
func (m *Manager) Retry(id string) (model.Job, error) {
	m.mu.Lock()
	canonical, ok := m.resolveIDLocked(id)
	if !ok {
		m.mu.Unlock()
		return model.Job{}, ErrNotFound
	}
	j := m.jobs[canonical]
	switch j.State {
	case model.StateQueued, model.StateSearching, model.StateFound:
		m.mu.Unlock()
		return model.Job{}, ErrNotRetryable
	}
	now := time.Now()
	j.State = model.StateQueued
	j.Error = ""
	j.Warning = ""
	j.Attempts = 0
	j.StartedAt = nil
	j.FinishedAt = nil
	j.NextAttemptAt = nil
	j.UpdatedAt = now
	if err := m.store.Put(*j); err != nil {
		m.log.Error("failed to persist retried job", "id", canonical, "error", err)
	}
	m.enqueueLocked(canonical)
	cp := *j
	m.mu.Unlock()
	m.signal()
	m.log.Info("job queued for retry", "id", canonical, "state", cp.State)
	return cp, nil
}

// Delete cancels (if needed) and removes a job. With purge it also deletes the
// saved .torrent file.
func (m *Manager) Delete(id string, purge bool) error {
	m.mu.Lock()
	canonical, ok := m.resolveIDLocked(id)
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	j := m.jobs[canonical]
	path := j.Path
	act, isActive := m.active[canonical]
	delete(m.jobs, canonical)
	delete(m.queued, canonical)
	for alias, target := range m.aliases {
		if target == canonical {
			delete(m.aliases, alias)
		}
	}
	if err := m.store.Delete(canonical); err != nil {
		m.log.Error("failed to delete job from store", "id", canonical, "error", err)
	}
	if purge {
		m.unindexPathLocked(path)
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
	m.log.Info("job removed", "id", canonical, "purge", purge)
	return nil
}

// TorrentFile returns the path and suggested file name for a job with saved
// metadata. Incomplete v2/hybrid torrents are downloadable too.
func (m *Manager) TorrentFile(id string) (path, name string, err error) {
	m.mu.Lock()
	canonical, ok := m.resolveIDLocked(id)
	if !ok {
		m.mu.Unlock()
		return "", "", ErrNotFound
	}
	j := m.jobs[canonical]
	if j.Path == "" {
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
	Total      int `json:"total"`
	Queued     int `json:"queued"`
	Searching  int `json:"searching"`
	Found      int `json:"found"`
	Incomplete int `json:"incomplete"`
	Failed     int `json:"failed"`
	Canceled   int `json:"canceled"`
	Active     int `json:"active"`
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
		case model.StateIncomplete:
			s.Incomplete++
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
		j.Warning = ""
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
	if err == nil && !metadataMatchesJob(md, att.job.ID) {
		err = fmt.Errorf("metadata hashes do not match job %s (v1=%s v2=%s)", att.job.ID, md.InfoHashV1, md.InfoHashV2)
	}
	if err == nil {
		m.finishMetadata(att, md)
		return
	}
	m.finishFailed(att, err)
}

func metadataMatchesJob(md *metafetch.Metadata, id string) bool {
	return id == md.InfoHash || id == md.InfoHashV1 || id == md.InfoHashV2
}

func (m *Manager) finishMetadata(att attempt, md *metafetch.Metadata) {
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
	state := model.StateFound
	warning := ""
	if md.Version != model.VersionV1 && !md.PieceLayersComplete {
		state = model.StateIncomplete
		warning = md.Warning
	}
	now := time.Now()
	j.State = state
	j.Name = md.Name
	j.Path = path
	j.Length = md.Length
	j.Files = md.Files
	j.InfoHash = j.ID
	j.InfoHashV1 = md.InfoHashV1
	j.InfoHashV2 = md.InfoHashV2
	j.Version = md.Version
	j.Error = ""
	j.Warning = warning
	j.NextAttemptAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	m.addJobAliasesLocked(j)
	m.indexFileLocked(path)
	target := m.mergeClusterLocked(j)
	m.log.Info("metadata found",
		"id", target.ID,
		"version", target.Version,
		"state", target.State,
		"name", target.Name,
		"length", target.Length,
		"files", target.Files,
		"path", target.Path,
		"piece_layers_complete", target.State == model.StateFound || target.Version == model.VersionV1,
		"attempt", target.Attempts,
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
		j.Warning = ""
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
	j.Warning = ""
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
