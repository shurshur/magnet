package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func torrentBytes(t *testing.T, name string, length int64) []byte {
	t.Helper()
	infoBytes, err := bencode.Marshal(metainfo.Info{
		PieceLength: 16 * 1024,
		Name:        name,
		Length:      length,
		Pieces:      make([]byte, 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hashOf(data []byte) string {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	return mi.HashInfoBytes().HexString()
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Ubuntu 24.04.iso", "Ubuntu 24.04.iso"},
		{"../etc/passwd", "_etc_passwd"},
		{`a\b:c*d?e"f<g>h|i`, "a_b_c_d_e_f_g_h_i"},
		{"..", ""},
		{"   ", ""},
		{".hidden", "hidden"},
		{"name.torrent", "name.torrent"},
	}
	for _, tc := range tests {
		if got := SanitizeFileName(tc.in); got != tc.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTorrentBaseName(t *testing.T) {
	if got := TorrentBaseName("Movie", "abc"); got != "Movie.torrent" {
		t.Fatalf("got %q", got)
	}
	if got := TorrentBaseName("movie.TORRENT", "abc"); got != "movie.TORRENT" {
		t.Fatalf("got %q", got)
	}
	if got := TorrentBaseName("", "abc123"); got != "abc123.torrent" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveOutputPathNewDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "downloads")
	path, exists, err := ResolveOutputPath(dir, "My Torrent", "deadbeef", false)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("new file should not exist")
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("path %q is not inside %q", path, dir)
	}
	if !strings.HasSuffix(path, "My Torrent.torrent") {
		t.Fatalf("unexpected path %q", path)
	}
}

func TestResolveOutputPathExactFileAndSameHash(t *testing.T) {
	dir := t.TempDir()
	data := torrentBytes(t, "existing", 1024)
	hash := hashOf(data)
	path := filepath.Join(dir, "explicit.torrent")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, exists, err := ResolveOutputPath(path, "ignored", hash, false)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected existing file with the same hash to be reused")
	}
	if got != path {
		t.Fatalf("got %q, want %q", got, path)
	}

	otherHash := strings.Repeat("a", 40)
	if _, _, err := ResolveOutputPath(path, "ignored", otherHash, false); err == nil {
		t.Fatal("expected collision error for explicit path")
	}
}

func TestResolveOutputPathCollisionGetsSuffix(t *testing.T) {
	dir := t.TempDir()
	first := torrentBytes(t, "same name", 100)
	second := torrentBytes(t, "same name", 200)
	if err := os.WriteFile(filepath.Join(dir, "same name.torrent"), first, 0o644); err != nil {
		t.Fatal(err)
	}
	secondHash := hashOf(second)
	path, exists, err := ResolveOutputPath(dir, "same name", secondHash, false)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("did not expect a match")
	}
	wantSuffix := secondHash[:8] + ".torrent"
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("got %q, want suffix %q", path, wantSuffix)
	}
}

func TestAtomicWriteReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "file.torrent")
	if err := AtomicWriteReader(path, strings.NewReader("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}
