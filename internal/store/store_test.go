package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shurshur/magnet/internal/model"
)

func TestStorePutLoadDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	jobs := []model.Job{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Magnet: "magnet:a", InfoHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: model.StateQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Magnet: "magnet:b", InfoHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: model.StateFound, CreatedAt: now, UpdatedAt: now},
	}
	if err := s.Put(jobs...); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d jobs", len(loaded))
	}
	if err := s.Delete(jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != jobs[1].ID {
		t.Fatalf("unexpected jobs after delete: %+v", loaded)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must preserve data.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	loaded, err = s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != jobs[1].ID {
		t.Fatalf("unexpected jobs after reopen: %+v", loaded)
	}
}
