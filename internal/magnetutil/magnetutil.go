// Package magnetutil validates and normalizes magnet links and info hashes.
package magnetutil

import (
	"errors"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// ErrEmpty is returned for an empty magnet link.
var ErrEmpty = errors.New("magnet link is empty")

// Parse validates a magnet link and returns its lowercase hex v1 info hash.
// Magnet links without a v1 (btih) info hash are rejected.
func Parse(link string) (id string, canonical string, err error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return "", "", ErrEmpty
	}
	m, err := metainfo.ParseMagnetUri(link)
	if err != nil {
		return "", "", fmt.Errorf("invalid magnet link: %w", err)
	}
	return m.InfoHash.HexString(), link, nil
}

// NormalizeID lowercases and validates a job ID (a 40 character hex info hash).
func NormalizeID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 40 {
		return "", fmt.Errorf("invalid job id %q: expected 40 hex characters", id)
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("invalid job id %q: expected 40 hex characters", id)
		}
	}
	return id, nil
}
