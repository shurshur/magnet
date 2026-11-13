package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shurshur/magnet/internal/api"
	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
	"github.com/shurshur/magnet/internal/store"
)

// DefaultDaemonURL is probed when --server is not supplied.
const DefaultDaemonURL = "http://127.0.0.1:8787"

// BackendInfo describes the selected backend for the UI.
type BackendInfo struct {
	Mode    string `json:"mode"`
	DataDir string `json:"data_dir,omitempty"`
	Server  string `json:"server,omitempty"`
}

// Backend is the common interface used by the web UI handlers. It is implemented
// both by an in-process daemon manager and by a remote daemon API client.
type Backend interface {
	Info() BackendInfo
	List(ctx context.Context, opts daemon.ListOptions) ([]model.Job, int, error)
	Add(ctx context.Context, magnets []string) ([]model.Job, int, int, error)
	Get(ctx context.Context, id string) (model.Job, error)
	Cancel(ctx context.Context, id string) (model.Job, error)
	Retry(ctx context.Context, id string) (model.Job, error)
	Delete(ctx context.Context, id string, purge bool) error
	OpenTorrent(ctx context.Context, id string) (io.ReadCloser, string, error)
	Stats(ctx context.Context) (daemon.Stats, int64, error)
	Close()
}

type embeddedBackend struct {
	mgr       *daemon.Manager
	store     *store.Store
	fetcher   *metafetch.Fetcher
	cancel    context.CancelFunc
	dataDir   string
	closeOnce sync.Once
}

func newEmbeddedBackend(ctx context.Context, opts Options, log *slog.Logger) (*embeddedBackend, error) {
	root, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	torrentDir := filepath.Join(root, "torrents")
	cacheDir := filepath.Join(root, "cache")
	for _, dir := range []string{root, torrentDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		return nil, fmt.Errorf("open state database: %w (if a daemon is already running, use --server)", err)
	}
	fetcher, err := metafetch.New(metafetch.Config{
		DataDir:         cacheDir,
		ListenPort:      opts.Port,
		DisableTrackers: opts.DisableTrackers,
		Logger:          log,
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	mgr, err := daemon.New(daemon.Config{
		Store:          st,
		Fetcher:        fetcher,
		OutputDir:      torrentDir,
		Workers:        opts.Workers,
		AttemptTimeout: opts.AttemptTimeout,
		RetryDelay:     opts.RetryDelay,
		MaxAttempts:    opts.MaxAttempts,
		Logger:         log,
	})
	if err != nil {
		fetcher.Close()
		st.Close()
		return nil, err
	}
	mctx, cancel := context.WithCancel(ctx)
	if err := mgr.Start(mctx); err != nil {
		cancel()
		fetcher.Close()
		st.Close()
		return nil, err
	}
	return &embeddedBackend{
		mgr:     mgr,
		store:   st,
		fetcher: fetcher,
		cancel:  cancel,
		dataDir: root,
	}, nil
}

func (b *embeddedBackend) Info() BackendInfo {
	return BackendInfo{Mode: "embedded", DataDir: b.dataDir}
}

func (b *embeddedBackend) List(_ context.Context, opts daemon.ListOptions) ([]model.Job, int, error) {
	return b.mgr.List(opts)
}

func (b *embeddedBackend) Add(_ context.Context, magnets []string) ([]model.Job, int, int, error) {
	jobs, created, err := b.mgr.AddBatch(magnets)
	return jobs, created, len(jobs) - created, err
}

func (b *embeddedBackend) Get(_ context.Context, id string) (model.Job, error) {
	return b.mgr.Get(id)
}

func (b *embeddedBackend) Cancel(_ context.Context, id string) (model.Job, error) {
	return b.mgr.Cancel(id)
}

func (b *embeddedBackend) Retry(_ context.Context, id string) (model.Job, error) {
	return b.mgr.Retry(id)
}

func (b *embeddedBackend) Delete(_ context.Context, id string, purge bool) error {
	return b.mgr.Delete(id, purge)
}

func (b *embeddedBackend) OpenTorrent(_ context.Context, id string) (io.ReadCloser, string, error) {
	path, name, err := b.mgr.TorrentFile(id)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	return f, fsutil.TorrentBaseName(name, id), nil
}

func (b *embeddedBackend) Stats(_ context.Context) (daemon.Stats, int64, error) {
	return b.mgr.Stats(), int64(b.mgr.Uptime().Seconds()), nil
}

func (b *embeddedBackend) Close() {
	b.closeOnce.Do(func() {
		b.cancel()
		b.mgr.Close()
		if err := b.fetcher.Close(); err != nil {
			// The web UI is shutting down; there is nothing useful to do here.
		}
		b.store.Close()
	})
}

type clientBackend struct {
	client *api.Client
}

func (b *clientBackend) Info() BackendInfo {
	return BackendInfo{Mode: "daemon", Server: b.client.BaseURL}
}

func (b *clientBackend) List(ctx context.Context, opts daemon.ListOptions) ([]model.Job, int, error) {
	res, err := b.client.List(ctx, opts.State, opts.Limit, opts.Offset)
	return res.Jobs, res.Total, err
}

func (b *clientBackend) Add(ctx context.Context, magnets []string) ([]model.Job, int, int, error) {
	const chunkSize = 500
	var jobs []model.Job
	created, existing := 0, 0
	for start := 0; start < len(magnets); start += chunkSize {
		end := start + chunkSize
		if end > len(magnets) {
			end = len(magnets)
		}
		res, err := b.client.Add(ctx, magnets[start:end])
		if err != nil {
			return nil, 0, 0, err
		}
		jobs = append(jobs, res.Jobs...)
		created += res.Created
		existing += res.Existing
	}
	return jobs, created, existing, nil
}

func (b *clientBackend) Get(ctx context.Context, id string) (model.Job, error) {
	return b.client.Get(ctx, id)
}

func (b *clientBackend) Cancel(ctx context.Context, id string) (model.Job, error) {
	return b.client.Cancel(ctx, id)
}

func (b *clientBackend) Retry(ctx context.Context, id string) (model.Job, error) {
	return b.client.Retry(ctx, id)
}

func (b *clientBackend) Delete(ctx context.Context, id string, purge bool) error {
	return b.client.Delete(ctx, id, purge)
}

func (b *clientBackend) OpenTorrent(ctx context.Context, id string) (io.ReadCloser, string, error) {
	return b.client.OpenTorrent(ctx, id)
}

func (b *clientBackend) Stats(ctx context.Context) (daemon.Stats, int64, error) {
	resp, err := b.client.Stats(ctx)
	return resp.Stats, resp.UptimeSeconds, err
}

func (b *clientBackend) Close() {}

// OpenBackend selects an in-process engine or a running daemon.
func OpenBackend(ctx context.Context, opts Options, log *slog.Logger) (Backend, error) {
	if opts.Server != "" && opts.Embedded {
		return nil, errors.New("--server and --embedded cannot be used together")
	}
	if opts.Server != "" {
		client := api.NewClient(opts.Server, opts.APIToken)
		if err := checkDaemon(ctx, client); err != nil {
			return nil, err
		}
		log.Info("connected to running daemon", "server", client.BaseURL)
		return &clientBackend{client: client}, nil
	}
	if !opts.Embedded {
		client, err := probeDefaultDaemon(ctx, opts.APIToken)
		if err != nil {
			return nil, err
		}
		if client != nil {
			log.Info("found running daemon; using it", "server", client.BaseURL)
			return &clientBackend{client: client}, nil
		}
	}
	log.Info("starting embedded engine", "data_dir", opts.DataDir)
	return newEmbeddedBackend(ctx, opts, log)
}

func probeDefaultDaemon(ctx context.Context, token string) (*api.Client, error) {
	client := api.NewClient(DefaultDaemonURL, token)
	hctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	_, err := client.Health(hctx)
	cancel()
	if err != nil {
		return nil, nil // nothing is listening; start the embedded engine
	}
	if err := checkDaemon(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

func checkDaemon(ctx context.Context, client *api.Client) error {
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := client.Health(hctx); err != nil {
		return fmt.Errorf("connect to daemon %s: %w", client.BaseURL, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := client.Stats(sctx); err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 401 {
			return fmt.Errorf("daemon %s requires an API token; use --api-token or MAGNET_API_TOKEN", client.BaseURL)
		}
		return fmt.Errorf("check daemon %s: %w", client.BaseURL, err)
	}
	return nil
}
