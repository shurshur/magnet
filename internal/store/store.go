// Package store persists daemon jobs in an embedded bbolt database.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/shurshur/magnet/internal/model"
)

var jobsBucket = []byte("jobs")

// Store is a small, crash-safe job database.
type Store struct {
	db *bolt.DB
}

// Open opens (and creates if needed) a bbolt database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(jobsBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("init state database: %w", err)
	}
	return &Store{db: db}, nil
}

// LoadAll returns every job stored in the database.
func (s *Store) LoadAll() ([]model.Job, error) {
	var jobs []model.Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		return b.ForEach(func(k, v []byte) error {
			var j model.Job
			if err := json.Unmarshal(v, &j); err != nil {
				return fmt.Errorf("decode job %q: %w", string(k), err)
			}
			jobs = append(jobs, j)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("load jobs: %w", err)
	}
	return jobs, nil
}

// Put stores one or more jobs atomically.
func (s *Store) Put(jobs ...model.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		for _, j := range jobs {
			data, err := json.Marshal(j)
			if err != nil {
				return fmt.Errorf("encode job %q: %w", j.ID, err)
			}
			if err := b.Put([]byte(j.ID), data); err != nil {
				return fmt.Errorf("store job %q: %w", j.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save jobs: %w", err)
	}
	return nil
}

// Delete removes a job.
func (s *Store) Delete(id string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(jobsBucket).Delete([]byte(id))
	})
	if err != nil {
		return fmt.Errorf("delete job %q: %w", id, err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}
