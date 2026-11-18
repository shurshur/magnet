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
	// StateIncomplete means metadata was saved, but v2 piece layers are missing.
	// The file is still usable for v1 clients and can be downloaded.
	StateIncomplete State = "incomplete"
	// StateFailed means the job exhausted its attempts or failed permanently.
	StateFailed State = "failed"
	// StateCanceled means the user canceled the job.
	StateCanceled State = "canceled"
)

// Torrent versions.
const (
	VersionV1     = "v1"
	VersionV2     = "v2"
	VersionHybrid = "hybrid"
)

// Job describes a single magnet link lookup.
type Job struct {
	// ID is the canonical identifier: the v1 info hash for v1/hybrid torrents,
	// or the v2 info hash for v2-only torrents.
	ID string `json:"id"`
	// Magnet is the first magnet link used to create the job.
	Magnet string `json:"magnet"`
	// Magnets contains all distinct magnet links that resolved to this job.
	Magnets []string `json:"magnets,omitempty"`
	// Aliases contains alternate IDs and info hashes resolving to this job.
	Aliases []string `json:"aliases,omitempty"`

	// InfoHash is kept equal to ID for backwards compatibility.
	InfoHash   string `json:"info_hash"`
	InfoHashV1 string `json:"info_hash_v1,omitempty"`
	InfoHashV2 string `json:"info_hash_v2,omitempty"`
	Version    string `json:"version,omitempty"`

	Name  string `json:"name,omitempty"`
	State State  `json:"state"`

	// Path is the absolute path of the saved .torrent file.
	Path string `json:"path,omitempty"`
	// Length is the total size of the torrent contents in bytes.
	Length int64 `json:"length,omitempty"`
	// Files is the number of files in the torrent.
	Files int `json:"files,omitempty"`

	// Error contains the last error, or a retry notice for queued jobs.
	Error string `json:"error,omitempty"`
	// Warning contains non-fatal metadata completeness information.
	Warning string `json:"warning,omitempty"`
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
	return j.State == StateFound || j.State == StateIncomplete ||
		j.State == StateFailed || j.State == StateCanceled
}

// Downloadable reports whether the saved .torrent can be served.
func (j Job) Downloadable() bool {
	return j.State == StateFound || j.State == StateIncomplete
}
