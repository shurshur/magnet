package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/magnetutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
	"github.com/shurshur/magnet/internal/store"
)

var (
	testTorrentData = buildTestTorrent()
	testInfoHash    = func() string {
		mi, err := metainfo.Load(bytes.NewReader(testTorrentData))
		if err != nil {
			panic(err)
		}
		return mi.HashInfoBytes().HexString()
	}()
	testMagnetLink = "magnet:?xt=urn:btih:" + testInfoHash
)

func buildTestTorrent() []byte {
	infoBytes, err := bencode.Marshal(metainfo.Info{
		PieceLength: 16 * 1024,
		Name:        "api-test",
		Length:      1000,
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

type fakeFetcher struct{}

func (fakeFetcher) Fetch(_ context.Context, magnet string) (*metafetch.Metadata, error) {
	parsed, _ := magnetutil.Parse(magnet)
	return &metafetch.Metadata{
		InfoHash:            parsed.PrimaryID,
		InfoHashV1:          parsed.InfoHashV1,
		InfoHashV2:          parsed.InfoHashV2,
		Version:             parsed.Version,
		Name:                "api-test",
		Length:              1000,
		Files:               1,
		PieceLayersComplete: true,
		Torrent:             testTorrentData,
	}, nil
}

func newTestServer(t *testing.T, token string) (*httptest.Server, *daemon.Manager) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := daemon.New(daemon.Config{
		Store:          st,
		Fetcher:        fakeFetcher{},
		OutputDir:      filepath.Join(dir, "torrents"),
		Workers:        2,
		AttemptTimeout: 5 * time.Second,
		RetryDelay:     10 * time.Millisecond,
	})
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
	api, err := New(Config{Manager: mgr, Token: token, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		ts.Close()
		stop()
		mgr.Close()
		st.Close()
	})
	return ts, mgr
}

func waitFound(t *testing.T, c *Client, id string) model.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == model.StateFound {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s was not found", id)
	return model.Job{}
}

func TestAPIFlow(t *testing.T) {
	ts, _ := newTestServer(t, "")
	c := NewClient(ts.URL, "")

	res, err := c.Add(context.Background(), []string{testMagnetLink})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || len(res.Jobs) != 1 {
		t.Fatalf("unexpected add result: %+v", res)
	}
	id := res.Jobs[0].ID
	j := waitFound(t, c, id)
	if j.Name != "api-test" || j.Path == "" {
		t.Fatalf("job = %+v", j)
	}

	again, err := c.Add(context.Background(), []string{testMagnetLink})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created != 0 || again.Existing != 1 {
		t.Fatalf("unexpected duplicate result: %+v", again)
	}

	list, err := c.List(context.Background(), model.StateFound, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Jobs) != 1 {
		t.Fatalf("list = %+v", list)
	}

	body, filename, err := c.OpenTorrent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if filename != "api-test.torrent" {
		t.Fatalf("filename = %q", filename)
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if mi.HashInfoBytes().HexString() != id {
		t.Fatalf("torrent hash mismatch")
	}

	stats, err := c.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stats.Found != 1 {
		t.Fatalf("stats = %+v", stats.Stats)
	}
}

func TestAPIInvalidMagnet(t *testing.T) {
	ts, _ := newTestServer(t, "")
	c := NewClient(ts.URL, "")
	_, err := c.Add(context.Background(), []string{"not-a-magnet"})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "invalid_magnet" {
		t.Fatalf("error = %+v", apiErr)
	}
}

func TestAPIAuthentication(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	noToken := NewClient(ts.URL, "")
	if _, err := noToken.List(context.Background(), "", 10, 0); err == nil {
		t.Fatal("expected unauthorized error")
	} else if apiErr, ok := err.(*Error); !ok || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected error: %v", err)
	}

	// Health stays public for monitoring.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	withToken := NewClient(ts.URL, "secret")
	if _, err := withToken.List(context.Background(), "", 10, 0); err != nil {
		t.Fatalf("authorized request failed: %v", err)
	}
}

func TestAPIJobLifecycle(t *testing.T) {
	ts, _ := newTestServer(t, "")
	c := NewClient(ts.URL, "")
	res, err := c.Add(context.Background(), []string{testMagnetLink})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Jobs[0].ID
	waitFound(t, c, id)

	if err := c.Delete(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), id); err == nil {
		t.Fatal("expected not found after delete")
	}
	if _, err := c.Cancel(context.Background(), id); err == nil {
		t.Fatal("expected not found after delete")
	}
}
