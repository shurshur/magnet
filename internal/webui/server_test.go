package webui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/model"
)

type fakeBackend struct {
	mu       sync.Mutex
	jobs     []model.Job
	canceled []string
	deleted  []string
	torrent  []byte
}

func newFakeBackend() *fakeBackend {
	now := time.Now()
	return &fakeBackend{
		jobs: []model.Job{{
			ID:        "0123456789abcdef0123456789abcdef01234567",
			Magnet:    "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
			InfoHash:  "0123456789abcdef0123456789abcdef01234567",
			Name:      "example",
			State:     model.StateFound,
			Length:    1024,
			Files:     1,
			Attempts:  1,
			CreatedAt: now,
			UpdatedAt: now,
		}},
		torrent: []byte("d4:infod4:name7:exampleee"),
	}
}

func (b *fakeBackend) Info() BackendInfo {
	return BackendInfo{Mode: "embedded", DataDir: "/tmp/magnet"}
}

func (b *fakeBackend) List(_ context.Context, opts daemon.ListOptions) ([]model.Job, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []model.Job
	for _, j := range b.jobs {
		if opts.State == "" || j.State == opts.State {
			out = append(out, j)
		}
	}
	total := len(out)
	return out, total, nil
}

func (b *fakeBackend) Add(_ context.Context, magnets []string) ([]model.Job, int, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var added []model.Job
	for i, magnet := range magnets {
		if strings.TrimSpace(magnet) == "" {
			return nil, 0, 0, &daemon.ValidationError{Link: magnet, Err: errors.New("invalid magnet link")}
		}
		j := model.Job{
			ID:        strings.Repeat("a", 39) + string(rune('0'+i)),
			Magnet:    magnet,
			InfoHash:  strings.Repeat("a", 40),
			State:     model.StateQueued,
			CreatedAt: now,
			UpdatedAt: now,
		}
		b.jobs = append(b.jobs, j)
		added = append(added, j)
	}
	return added, len(added), 0, nil
}

func (b *fakeBackend) Get(_ context.Context, id string) (model.Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, j := range b.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return model.Job{}, daemon.ErrNotFound
}

func (b *fakeBackend) Cancel(_ context.Context, id string) (model.Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.canceled = append(b.canceled, id)
	return model.Job{ID: id, State: model.StateCanceled}, nil
}

func (b *fakeBackend) Retry(_ context.Context, id string) (model.Job, error) {
	return model.Job{ID: id, State: model.StateQueued}, nil
}

func (b *fakeBackend) Delete(_ context.Context, id string, purge bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, id)
	return nil
}

func (b *fakeBackend) OpenTorrent(_ context.Context, id string) (io.ReadCloser, string, error) {
	if id != "0123456789abcdef0123456789abcdef01234567" {
		return nil, "", daemon.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b.torrent)), "example.torrent", nil
}

func (b *fakeBackend) Stats(context.Context) (daemon.Stats, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return daemon.Stats{Total: len(b.jobs), Found: len(b.jobs)}, 42, nil
}

func (b *fakeBackend) Close() {}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	s, err := NewServer(newFakeBackend(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func TestWebUIStaticAndInfo(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "magnet") {
		t.Fatal("index page does not look like the web UI")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("missing content security policy")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/info", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"embedded"`) {
		t.Fatalf("info response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWebUIMutatingRequestsRequireHeader(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status without header = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Magnet-Web", "1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWebUIActionsAndTorrent(t *testing.T) {
	h := newTestHandler(t)
	id := "0123456789abcdef0123456789abcdef01234567"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+id+"/torrent", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("torrent status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "d4:infod4:name7:exampleee" {
		t.Fatalf("torrent body = %q", got)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "example.torrent") {
		t.Fatalf("content disposition = %q", rec.Header().Get("Content-Disposition"))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/jobs/"+id+"/cancel", nil)
	req.Header.Set("X-Magnet-Web", "1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/jobs/"+id, nil)
	req.Header.Set("X-Magnet-Web", "1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
}

func TestWebUIRejectsCrossOriginMutation(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/0123456789abcdef0123456789abcdef01234567/cancel", nil)
	req.Header.Set("X-Magnet-Web", "1")
	req.Header.Set("Origin", "http://evil.example")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", rec.Code)
	}
}
