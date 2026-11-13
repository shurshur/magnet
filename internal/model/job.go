// Package model contains data types shared by the daemon, the API and the CLI.
package model

import "time"

// State is the lifecycle state of a metadata lookup job.
type State string

const (
	// StateQueued means the job is waiting for a free worker.
	StateQueued State = "queued"
	// StateSearching means metadata is currently being fetched from the DHT.
	StateSearching State = "searching"
	// StateFound means the .torrent file was successfully saved.
	StateFound State = "found"
	// StateFailed means the job exhausted its attempts or failed permanently.
	StateFailed State = "failed"
	// StateCanceled means the user canceled the job.
	StateCanceled State = "canceled"
)

// Job describes a single magnet link lookup.
type Job struct {
	// ID is the lowercase hex v1 info hash. It doubles as a stable identifier.
	ID       string `json:"id"`
	Magnet   string `json:"magnet"`
	InfoHash string `json:"info_hash"`
	Name     string `json:"name,omitempty"`
	State    State  `json:"state"`

	// Path is the absolute path of the saved .torrent file (when State is found).
	Path string `json:"path,omitempty"`
	// Length is the total size of the torrent contents in bytes.
	Length int64 `json:"length,omitempty"`
	// Files is the number of files in the torrent.
	Files int `json:"files,omitempty"`

	// Error contains the last error, or a retry notice for queued jobs.
	Error string `json:"error,omitempty"`
	// Attempts is the number of started search attempts.
	Attempts int `json:"attempts"`

	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

// Terminal reports whether the job will not change state on its own.
func (j Job) Terminal() bool {
	return j.State == StateFound || j.State == StateFailed || j.State == StateCanceled
}
