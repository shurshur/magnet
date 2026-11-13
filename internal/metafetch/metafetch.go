// Package metafetch retrieves BitTorrent metadata (.torrent files) from the
// DHT using magnet links.
package metafetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// Config controls the metadata fetcher.
type Config struct {
	// DataDir is used by the torrent client for its (empty) storage.
	DataDir string
	// ListenPort is the UDP/TCP port used for DHT and peer connections.
	// Zero picks a random free port.
	ListenPort int
	// DisableTrackers restricts peer discovery to the DHT and magnet peers.
	DisableTrackers bool
	// Logger receives diagnostic messages. Optional.
	Logger *slog.Logger
}

// Metadata is a successfully retrieved torrent metainfo.
type Metadata struct {
	InfoHash string
	Name     string
	Length   int64
	Files    int
	// Torrent is the bencoded .torrent file.
	Torrent []byte
}

// Fetcher wraps a torrent client that is used for metadata lookups only.
// It is safe to use concurrently.
type Fetcher struct {
	client *torrent.Client
	log    *slog.Logger
}

// New creates a fetcher backed by the DHT.
func New(cfg Config) (*Fetcher, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("metadata fetcher: data directory is required")
	}
	cc := torrent.NewDefaultClientConfig()
	cc.DataDir = cfg.DataDir
	cc.ListenPort = cfg.ListenPort
	cc.NoUpload = true
	cc.Seed = false
	cc.DisableTrackers = cfg.DisableTrackers
	// Metadata exchange happens over established peer connections. Keeping
	// connections open is what lets the DHT lookup turn into a metadata fetch.
	cc.AcceptPeerConnections = true

	client, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("start torrent client: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Fetcher{client: client, log: log}, nil
}

// Fetch waits until the metadata for magnet is available and returns it.
// The call is canceled when ctx is done. The returned slice is a complete,
// bencoded .torrent file.
func (f *Fetcher) Fetch(ctx context.Context, magnet string) (*Metadata, error) {
	// Parsing up front rejects malformed links without touching the DHT.
	if _, err := metainfo.ParseMagnetUri(magnet); err != nil {
		return nil, fmt.Errorf("invalid magnet link: %w", err)
	}

	t, err := f.client.AddMagnet(magnet)
	if err != nil {
		return nil, fmt.Errorf("add magnet: %w", err)
	}
	defer t.Drop()

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for metadata: %w", ctx.Err())
	}

	info := t.Info()
	if info == nil {
		return nil, errors.New("torrent client reported metadata but info is nil")
	}

	mi := t.Metainfo()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return nil, fmt.Errorf("encode metainfo: %w", err)
	}
	if buf.Len() == 0 {
		return nil, errors.New("encoded metainfo is empty")
	}

	name := info.Name
	if name == "" {
		name = info.NameUtf8
	}
	f.log.Debug("metadata retrieved",
		"info_hash", t.InfoHash().HexString(),
		"name", name,
		"length", info.TotalLength(),
		"peers", len(t.PeerConns()),
	)

	return &Metadata{
		InfoHash: t.InfoHash().HexString(),
		Name:     name,
		Length:   info.TotalLength(),
		Files:    len(info.UpvertedFiles()),
		Torrent:  buf.Bytes(),
	}, nil
}

// Close shuts down the torrent client and its DHT node.
func (f *Fetcher) Close() error {
	var errs []error
	for _, err := range f.client.Close() {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
