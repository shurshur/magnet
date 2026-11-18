// Package magnetutil validates and normalizes magnet links and info hashes.
package magnetutil

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/shurshur/magnet/internal/model"
)

// ErrEmpty is returned for an empty magnet link.
var ErrEmpty = errors.New("magnet link is empty")

// Parsed is a validated magnet link with all known info hashes.
type Parsed struct {
	Link       string
	PrimaryID  string
	InfoHashV1 string
	InfoHashV2 string
	Version    string
}

// Hashes returns all known non-empty info hashes.
func (p Parsed) Hashes() []string {
	hashes := make([]string, 0, 2)
	if p.InfoHashV1 != "" {
		hashes = append(hashes, p.InfoHashV1)
	}
	if p.InfoHashV2 != "" && p.InfoHashV2 != p.InfoHashV1 {
		hashes = append(hashes, p.InfoHashV2)
	}
	return hashes
}

// Parse validates a magnet link and returns its v1/v2 info hashes.
func Parse(link string) (Parsed, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return Parsed{}, ErrEmpty
	}
	m, err := metainfo.ParseMagnetV2Uri(link)
	if err != nil {
		return Parsed{}, fmt.Errorf("invalid magnet link: %w", err)
	}
	p := Parsed{Link: link}
	if m.InfoHash.Ok {
		p.InfoHashV1 = m.InfoHash.Value.HexString()
	}
	if m.V2InfoHash.Ok {
		p.InfoHashV2 = m.V2InfoHash.Value.HexString()
	}
	switch {
	case p.InfoHashV1 != "" && p.InfoHashV2 != "":
		p.Version = model.VersionHybrid
		p.PrimaryID = p.InfoHashV1
	case p.InfoHashV1 != "":
		p.Version = model.VersionV1
		p.PrimaryID = p.InfoHashV1
	case p.InfoHashV2 != "":
		p.Version = model.VersionV2
		p.PrimaryID = p.InfoHashV2
	default:
		return Parsed{}, errors.New("magnet link has no btih or btmh info hash")
	}
	return p, nil
}

// NormalizeID lowercases and validates a job ID. It accepts a 40-character
// v1 hash, a 64-character v2 hash, or a 68-character btmh multihash.
func NormalizeID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) == 68 && strings.HasPrefix(id, "1220") {
		id = id[4:]
	}
	if len(id) != 40 && len(id) != 64 {
		return "", fmt.Errorf("invalid job id %q: expected 40 or 64 hex characters", id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("invalid job id %q: expected hexadecimal", id)
	}
	return id, nil
}
