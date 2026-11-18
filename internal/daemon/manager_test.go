package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/shurshur/magnet/internal/magnetutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
	"github.com/shurshur/magnet/internal/store"
)

const (
	testMagnetA = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	testMagnetB = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
)

type fakeFetcher struct {
	mu    sync.Mutex
	calls int
	fn    func(context.Context, string) (*metafetch.Metadata, error)
}

func (f *fakeFetcher) Fetch(ctx context.Context, magnet string) (*metafetch.Metadata, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, magnet)
	}
	return successfulMetadata(magnet), nil
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func successfulMetadata(magnet string) *metafetch.Metadata {
	parsed, _ := magnetutil.Parse(magnet)
	id := parsed.PrimaryID
	return &metafetch.Metadata{
		InfoHash:            id,
		InfoHashV1:          parsed.InfoHashV1,
		InfoHashV2:          parsed.InfoHashV2,
		Version:             parsed.Version,
		Name:                "torrent-" + id[:8],
		Length:              12345,
		Files:               1,
		PieceLayersComplete: true,
		Torrent:             testTorrentBytes("torrent-"+id[:8], 12345),
	}
}

func testTorrentBytes(name string, length int64) []byte {
	infoBytes, err := bencode.Marshal(metainfo.Info{
		PieceLength: 16 * 1024,
		Name:        name,
		Length:      length,
		Pieces:      make([]byte, 20),
	})
	if err != nil {
		panic(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

type testEnv struct {
	mgr  *Manager
	st   *store.Store
	dir  string
	stop context.CancelFunc
}

func newTestEnv(t *testing.T, fetcher Fetcher, tweak func(*Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Store:          st,
		Fetcher:        fetcher,
		OutputDir:      filepath.Join(dir, "torrents"),
		Workers:        2,
		AttemptTimeout: 5 * time.Second,
		RetryDelay:     10 * time.Millisecond,
		MaxAttempts:    0,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	mgr, err := New(cfg)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		stop()
		mgr.Close()
		st.Close()
		t.Fatal(err)
	}
	env := &testEnv{mgr: mgr, st: st, dir: dir, stop: stop}
	t.Cleanup(func() {
		env.stop()
		env.mgr.Close()
		env.st.Close()
	})
	return env
}

func waitForState(t *testing.T, mgr *Manager, id string, state model.State, timeout time.Duration) model.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, err := mgr.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == state {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := mgr.Get(id)
	t.Fatalf("job %s did not reach state %s; last state=%s error=%q", id, state, j.State, j.Error)
	return model.Job{}
}

func TestManagerFindsMetadataAndDeduplicates(t *testing.T) {
	env := newTestEnv(t, &fakeFetcher{}, nil)
	j, created, err := env.mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a new job")
	}
	j = waitForState(t, env.mgr, j.ID, model.StateFound, 3*time.Second)
	if j.Name != "torrent-01234567" || j.Length != 12345 || j.Files != 1 {
		t.Fatalf("unexpected job: %+v", j)
	}
	if _, err := os.Stat(j.Path); err != nil {
		t.Fatalf("torrent file was not saved: %v", err)
	}

	again, created, err := env.mgr.Add("magnet:?xt=urn:btih:" + strings.ToUpper("0123456789abcdef0123456789abcdef01234567"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("duplicate magnet should not create a job")
	}
	if again.ID != j.ID || again.State != model.StateFound {
		t.Fatalf("unexpected duplicate response: %+v", again)
	}

	jobs, total, err := env.mgr.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(jobs) != 1 {
		t.Fatalf("total=%d jobs=%d", total, len(jobs))
	}
}

func TestManagerBatchValidationIsAtomic(t *testing.T) {
	env := newTestEnv(t, &fakeFetcher{}, nil)
	_, _, err := env.mgr.AddBatch([]string{testMagnetA, "not-a-magnet"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	_, total, err := env.mgr.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("invalid batch must not add jobs, total=%d", total)
	}
}

func TestManagerRetriesUntilSuccess(t *testing.T) {
	f := &fakeFetcher{}
	var count int
	f.fn = func(ctx context.Context, magnet string) (*metafetch.Metadata, error) {
		count++
		if count == 1 {
			return nil, errors.New("boom")
		}
		return successfulMetadata(magnet), nil
	}
	env := newTestEnv(t, f, func(c *Config) {
		c.RetryDelay = 10 * time.Millisecond
	})
	j, _, err := env.mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	j = waitForState(t, env.mgr, j.ID, model.StateFound, 3*time.Second)
	if j.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", j.Attempts)
	}
	if f.callCount() != 2 {
		t.Fatalf("calls = %d, want 2", f.callCount())
	}
}

func TestManagerMaxAttempts(t *testing.T) {
	f := &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
		return nil, errors.New("boom")
	}}
	env := newTestEnv(t, f, func(c *Config) {
		c.MaxAttempts = 1
		c.RetryDelay = 10 * time.Millisecond
	})
	j, _, err := env.mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	j = waitForState(t, env.mgr, j.ID, model.StateFailed, 3*time.Second)
	if j.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", j.Attempts)
	}
	if !strings.Contains(j.Error, "boom") {
		t.Fatalf("error = %q", j.Error)
	}
	if _, err := env.mgr.Retry(j.ID); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	waitForState(t, env.mgr, j.ID, model.StateFailed, 3*time.Second)
}

func TestManagerCancelSearching(t *testing.T) {
	started := make(chan struct{}, 1)
	f := &fakeFetcher{fn: func(ctx context.Context, _ string) (*metafetch.Metadata, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	env := newTestEnv(t, f, func(c *Config) {
		c.AttemptTimeout = 0
	})
	j, _, err := env.mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not start")
	}
	waitForState(t, env.mgr, j.ID, model.StateSearching, 3*time.Second)
	canceled, err := env.mgr.Cancel(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != model.StateCanceled {
		t.Fatalf("state = %s", canceled.State)
	}
	time.Sleep(50 * time.Millisecond)
	got, err := env.mgr.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateCanceled {
		t.Fatalf("worker overwrote canceled job: %s", got.State)
	}
}

func TestManagerRequeuesOnShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	f := &fakeFetcher{fn: func(ctx context.Context, _ string) (*metafetch.Metadata, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	env := newTestEnv(t, f, func(c *Config) {
		c.AttemptTimeout = 0
	})
	j, _, err := env.mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not start")
	}
	waitForState(t, env.mgr, j.ID, model.StateSearching, 3*time.Second)
	env.stop()
	env.mgr.Close()

	got, err := env.mgr.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateQueued {
		t.Fatalf("state after shutdown = %s, want queued", got.State)
	}
	stored, err := env.st.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].State != model.StateQueued {
		t.Fatalf("persisted state after shutdown: %+v", stored)
	}
}

func TestManagerPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Store:          st,
		Fetcher:        &fakeFetcher{},
		OutputDir:      filepath.Join(dir, "torrents"),
		Workers:        1,
		AttemptTimeout: time.Second,
		RetryDelay:     10 * time.Millisecond,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, _, err := mgr.Add(testMagnetA)
	if err != nil {
		t.Fatal(err)
	}
	j = waitForState(t, mgr, j.ID, model.StateFound, 3*time.Second)
	stop()
	mgr.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	mgr2, err := New(Config{
		Store:          st2,
		Fetcher:        &fakeFetcher{},
		OutputDir:      filepath.Join(dir, "torrents"),
		Workers:        1,
		AttemptTimeout: time.Second,
		RetryDelay:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	if err := mgr2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close()
	got, err := mgr2.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateFound || got.Path != j.Path {
		t.Fatalf("restored job = %+v", got)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Fatalf("restored torrent file missing: %v", err)
	}
}

func TestManagerImportExistingTorrent(t *testing.T) {
	dir := t.TempDir()
	torrentDir := filepath.Join(dir, "torrents")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := testTorrentBytes("already-here", 42)
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	id := mi.HashInfoBytes().HexString()
	magnet := "magnet:?xt=urn:btih:" + id
	if err := os.WriteFile(filepath.Join(torrentDir, id+".torrent"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr, err := New(Config{
		Store: st,
		Fetcher: &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
			return nil, errors.New("unexpected fetch")
		}},
		OutputDir:      torrentDir,
		Workers:        1,
		AttemptTimeout: time.Second,
		RetryDelay:     time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	j, created, err := mgr.Add(magnet)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a job record")
	}
	if j.State != model.StateFound || j.Name != "already-here" || j.Path == "" {
		t.Fatalf("job = %+v", j)
	}
}

func buildTestV2Torrent(t *testing.T, hybrid, complete bool) (data []byte, info *metainfo.Info, v1, v2 string) {
	t.Helper()
	content := bytes.Repeat([]byte("magnet-test-"), 4000)
	pieceLength := int64(16 * 1024)
	var pieceHashes [][32]byte
	var layer bytes.Buffer
	for off := int64(0); off < int64(len(content)); off += pieceLength {
		end := off + pieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		h := merkle.NewHash()
		if _, err := h.Write(content[off:end]); err != nil {
			t.Fatal(err)
		}
		sum := h.SumMinLength(nil, int(pieceLength))
		var arr [32]byte
		copy(arr[:], sum)
		pieceHashes = append(pieceHashes, arr)
		layer.Write(sum)
	}
	root := merkle.RootWithPadHash(pieceHashes, metainfo.HashForPiecePad(pieceLength))
	name := "hybrid.bin"
	info = &metainfo.Info{
		PieceLength: pieceLength,
		Name:        name,
		MetaVersion: 2,
		FileTree: metainfo.FileTree{File: metainfo.FileTreeFile{
			Length:     int64(len(content)),
			PiecesRoot: string(root[:]),
		}},
	}
	if hybrid {
		info.Length = int64(len(content))
		pieces, err := metainfo.GeneratePieces(bytes.NewReader(content), pieceLength, nil)
		if err != nil {
			t.Fatal(err)
		}
		info.Pieces = pieces
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	if complete {
		mi.PieceLayers = map[string]string{string(root[:]): layer.String()}
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if hybrid {
		v1 = mi.HashInfoBytes().HexString()
	}
	h := infohash_v2.HashBytes(mi.InfoBytes)
	v2 = h.HexString()
	return buf.Bytes(), info, v1, v2
}

func v1Link(hash string) string { return "magnet:?xt=urn:btih:" + hash }
func v2Link(hash string) string { return "magnet:?xt=urn:btmh:1220" + hash }

func TestManagerHybridIncompleteIsDownloadable(t *testing.T) {
	data, info, v1, v2 := buildTestV2Torrent(t, true, false)
	f := &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
		return &metafetch.Metadata{
			InfoHash:            v1,
			InfoHashV1:          v1,
			InfoHashV2:          v2,
			Version:             model.VersionHybrid,
			Name:                info.Name,
			Length:              info.TotalLength(),
			Files:               1,
			PieceLayersComplete: false,
			Warning:             "piece layers are incomplete",
			Torrent:             data,
		}, nil
	}}
	env := newTestEnv(t, f, nil)
	j, _, err := env.mgr.Add(v1Link(v1))
	if err != nil {
		t.Fatal(err)
	}
	j = waitForState(t, env.mgr, j.ID, model.StateIncomplete, 3*time.Second)
	if j.Version != model.VersionHybrid || j.Warning == "" {
		t.Fatalf("job = %+v", j)
	}
	path, _, err := env.mgr.TorrentFile(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := env.mgr.Retry(j.ID); err != nil {
		t.Fatalf("retry incomplete: %v", err)
	}
}

func TestManagerV2Incomplete(t *testing.T) {
	data, info, _, v2 := buildTestV2Torrent(t, false, false)
	f := &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
		return &metafetch.Metadata{
			InfoHash:            v2,
			InfoHashV2:          v2,
			Version:             model.VersionV2,
			Name:                info.Name,
			Length:              info.TotalLength(),
			Files:               1,
			PieceLayersComplete: false,
			Warning:             "piece layers are incomplete",
			Torrent:             data,
		}, nil
	}}
	env := newTestEnv(t, f, nil)
	j, _, err := env.mgr.Add(v2Link(v2))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.ID) != 64 {
		t.Fatalf("v2 id = %q", j.ID)
	}
	j = waitForState(t, env.mgr, j.ID, model.StateIncomplete, 3*time.Second)
	if j.Version != model.VersionV2 {
		t.Fatalf("version = %q", j.Version)
	}
	if _, _, err := env.mgr.TorrentFile(j.ID); err != nil {
		t.Fatalf("incomplete torrent is not downloadable: %v", err)
	}
}

func TestManagerMergesSeparateV1AndV2Jobs(t *testing.T) {
	data, info, v1, v2 := buildTestV2Torrent(t, true, true)
	f := &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
		return &metafetch.Metadata{
			InfoHash:            v1,
			InfoHashV1:          v1,
			InfoHashV2:          v2,
			Version:             model.VersionHybrid,
			Name:                info.Name,
			Length:              info.TotalLength(),
			Files:               1,
			PieceLayersComplete: true,
			Torrent:             data,
		}, nil
	}}
	env := newTestEnv(t, f, nil)
	jobs, created, err := env.mgr.AddBatch([]string{v1Link(v1), v2Link(v2)})
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 || len(jobs) != 2 {
		t.Fatalf("created=%d jobs=%d", created, len(jobs))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, total, err := env.mgr.List(ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if total == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	all, total, err := env.mgr.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(all) != 1 {
		t.Fatalf("jobs were not merged: total=%d jobs=%+v", total, all)
	}
	merged := all[0]
	if len(merged.Magnets) != 2 {
		t.Fatalf("magnets = %v", merged.Magnets)
	}
	byV1, err := env.mgr.Get(v1)
	if err != nil {
		t.Fatal(err)
	}
	byV2, err := env.mgr.Get(v2)
	if err != nil {
		t.Fatal(err)
	}
	if byV1.ID != byV2.ID || byV1.ID != merged.ID {
		t.Fatalf("alias resolution: v1=%s v2=%s merged=%s", byV1.ID, byV2.ID, merged.ID)
	}
	if byV1.State != model.StateFound {
		t.Fatalf("state = %s", byV1.State)
	}
}

func TestManagerReusesExistingTorrentFileForOtherHash(t *testing.T) {
	dir := t.TempDir()
	torrentDir := filepath.Join(dir, "torrents")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _, v1, v2 := buildTestV2Torrent(t, true, true)
	path := filepath.Join(torrentDir, v2+".torrent")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := &fakeFetcher{fn: func(context.Context, string) (*metafetch.Metadata, error) {
		return nil, errors.New("metadata fetch should not be called")
	}}
	mgr, err := New(Config{
		Store:          st,
		Fetcher:        f,
		OutputDir:      torrentDir,
		Workers:        1,
		AttemptTimeout: time.Second,
		RetryDelay:     time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	j, created, err := mgr.Add(v1Link(v1))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a new job")
	}
	if j.State != model.StateFound || j.Path != path {
		t.Fatalf("job = %+v", j)
	}
	if f.callCount() != 0 {
		t.Fatalf("fetch was called %d times", f.callCount())
	}
	if got, err := mgr.Get(v2); err != nil || got.ID != j.ID {
		t.Fatalf("v2 alias lookup = %+v, %v", got, err)
	}
}
