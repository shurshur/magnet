// Package fsutil contains small filesystem helpers shared by the CLI and the daemon.
package fsutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"
)

// AtomicWriteFile writes data to path using a temporary file in the same
// directory and an atomic rename.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return AtomicWriteReader(path, bytes.NewReader(data), perm)
}

// AtomicWriteReader streams r to path using a temporary file in the same
// directory and an atomic rename.
func AtomicWriteReader(path string, r io.Reader, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".magnet-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if _, err := io.Copy(f, r); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename temporary file to %s: %w", path, err)
	}
	return nil
}

// SanitizeFileName turns a torrent name into a safe single path component.
// It returns an empty string when nothing usable remains.
func SanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	// Leave room for a suffix and the .torrent extension on common filesystems.
	for len(name) > 180 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	name = strings.TrimRight(name, " .")
	return name
}

// TorrentBaseName returns a .torrent file name derived from a torrent's name,
// falling back to the info hash when the name is unusable.
func TorrentBaseName(name, infoHash string) string {
	base := SanitizeFileName(name)
	if base == "" {
		base = infoHash
	}
	if !strings.HasSuffix(strings.ToLower(base), ".torrent") {
		base += ".torrent"
	}
	return base
}

// ResolveOutputPath decides where a fetched .torrent file should be saved.
//
// out may be:
//   - an existing directory, or a path ending in a separator: a name derived
//     from the torrent is used inside it;
//   - any other path: it is treated as the exact target file.
//
// The returned exists flag reports that target already contains the same
// info hash, so the caller does not need to rewrite it. When a directory is
// used, existing files with the same hash are reused and name collisions with
// other torrents get a short hash suffix.
func ResolveOutputPath(out, name, infoHash string, force bool) (path string, exists bool, err error) {
	if out == "" {
		out = "."
	}
	// A .torrent suffix or an existing regular file means an exact file name.
	// Everything else is treated as a directory.
	dirMode := !strings.HasSuffix(strings.ToLower(out), ".torrent")
	if st, statErr := os.Stat(out); statErr == nil {
		dirMode = st.IsDir()
	}
	if !dirMode {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return "", false, fmt.Errorf("create output directory: %w", err)
		}
		if !force {
			if same, checkErr := sameTorrent(out, infoHash); checkErr == nil && same {
				return out, true, nil
			}
			if _, statErr := os.Stat(out); statErr == nil {
				return "", false, fmt.Errorf("output file %s already exists (use --force to overwrite)", out)
			}
		}
		return out, false, nil
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", false, fmt.Errorf("create output directory: %w", err)
	}
	base := TorrentBaseName(name, infoHash)

	// First try the plain name.
	candidate := filepath.Join(out, base)
	if force {
		return candidate, false, nil
	}
	if same, checkErr := sameTorrent(candidate, infoHash); checkErr == nil && same {
		return candidate, true, nil
	}
	if _, statErr := os.Stat(candidate); os.IsNotExist(statErr) {
		return candidate, false, nil
	}

	// The name is taken by a different torrent: add a short hash suffix.
	stem := strings.TrimSuffix(base, ".torrent")
	short := infoHash
	if len(short) > 8 {
		short = short[:8]
	}
	for i := 0; i < 1000; i++ {
		suffix := short
		if i > 0 {
			suffix = fmt.Sprintf("%s-%d", short, i+1)
		}
		candidate = filepath.Join(out, stem+"-"+suffix+".torrent")
		if same, checkErr := sameTorrent(candidate, infoHash); checkErr == nil && same {
			return candidate, true, nil
		}
		if _, statErr := os.Stat(candidate); os.IsNotExist(statErr) {
			return candidate, false, nil
		}
	}
	return "", false, fmt.Errorf("could not find a free file name for %s in %s", infoHash, out)
}

func sameTorrent(path, infoHash string) (bool, error) {
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return false, err
	}
	return mi.HashInfoBytes().HexString() == infoHash, nil
}
