package metafetch

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
)

func testV2Meta(t *testing.T, complete bool) metainfo.MetaInfo {
	t.Helper()
	content := bytes.Repeat([]byte("repair-test-"), 4000)
	pieceLength := int64(16 * 1024)
	var pieceHashes [][32]byte
	var layer bytes.Buffer
	for off := int64(0); off < int64(len(content)); off += pieceLength {
		end := off + pieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		h := merkle.NewHash()
		if _, err := h.Write(content[off:end]); err != nil {
			t.Fatal(err)
		}
		sum := h.SumMinLength(nil, int(pieceLength))
		var arr [32]byte
		copy(arr[:], sum)
		pieceHashes = append(pieceHashes, arr)
		layer.Write(sum)
	}
	root := merkle.RootWithPadHash(pieceHashes, metainfo.HashForPiecePad(pieceLength))
	pieces, err := metainfo.GeneratePieces(bytes.NewReader(content), pieceLength, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{
		PieceLength: pieceLength,
		Name:        "repair.bin",
		MetaVersion: 2,
		Length:      int64(len(content)),
		Pieces:      pieces,
		FileTree: metainfo.FileTree{File: metainfo.FileTreeFile{
			Length:     int64(len(content)),
			PiecesRoot: string(root[:]),
		}},
	}
	infoBytes, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	if complete {
		mi.PieceLayers = map[string]string{string(root[:]): layer.String()}
	}
	return mi
}

func TestRepairAlreadyComplete(t *testing.T) {
	f, err := New(Config{DataDir: t.TempDir(), DisableTrackers: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mi := testV2Meta(t, true)
	_, complete, warning, err := f.Repair(context.Background(), mi, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || warning != "" {
		t.Fatalf("complete=%v warning=%q", complete, warning)
	}
}

func TestRepairWithoutPeersTimesOut(t *testing.T) {
	f, err := New(Config{DataDir: t.TempDir(), DisableTrackers: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, complete, warning, err := f.Repair(ctx, testV2Meta(t, false), 0)
	if err != nil {
		t.Fatal(err)
	}
	if complete || warning == "" {
		t.Fatalf("complete=%v warning=%q", complete, warning)
	}
}
