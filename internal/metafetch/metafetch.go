// Package metafetch retrieves BitTorrent metadata (.torrent files) from the
// DHT using magnet links.
package metafetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/shurshur/magnet/internal/model"
)

// ErrIncompleteV2 is returned when a v2-only torrent has no piece layers and
// incomplete v2 metadata is not allowed.
var ErrIncompleteV2 = errors.New("v2 metadata is incomplete: piece layers are missing")

// TimeoutError describes a repair stage that exceeded its time budget.
type TimeoutError struct {
	Stage   string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Stage, e.Timeout)
}

func (e *TimeoutError) Unwrap() error { return context.DeadlineExceeded }

// Config controls the metadata fetcher.
type Config struct {
	// DataDir is used by the torrent client for its (empty) storage.
	DataDir string
	// ListenPort is the UDP/TCP port used for DHT and peer connections.
	// Zero picks a random free port.
	ListenPort int
	// DisableTrackers restricts peer discovery to the DHT and magnet peers.
	DisableTrackers bool
	// PieceLayersTimeout is how long to wait for BEP 52 piece layers after the
	// info dictionary arrives. Zero means one immediate check without waiting.
	PieceLayersTimeout time.Duration
	// AllowIncompleteV2 permits saving v2-only torrents without piece layers.
	// Hybrid torrents are always saved because their v1 part remains usable.
	AllowIncompleteV2 bool
	// Logger receives diagnostic messages. Optional.
	Logger *slog.Logger
}

// Metadata is a successfully retrieved torrent metainfo.
type Metadata struct {
	// InfoHash is the primary hash: v1 when available, otherwise v2.
	InfoHash   string
	InfoHashV1 string
	InfoHashV2 string
	Version    string
	Name       string
	Length     int64
	Files      int

	// PieceLayersComplete is true for v1-only torrents and for v2/hybrid
	// torrents whose BEP 52 piece layers were fully retrieved.
	PieceLayersComplete bool
	// Warning explains missing piece layers for incomplete metadata.
	Warning string
	// Torrent is the bencoded .torrent file. Partial piece layers are omitted.
	Torrent []byte
}

// Fetcher wraps a torrent client that is used for metadata lookups only.
// It is safe to use concurrently.
type Fetcher struct {
	client *torrent.Client
	config Config
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
	return &Fetcher{client: client, config: cfg, log: log}, nil
}

// Fetch waits until the metadata for magnet is available and returns it.
// The call is canceled when ctx is done. The returned slice is a complete,
// bencoded .torrent file when PieceLayersComplete is true.
func (f *Fetcher) Fetch(ctx context.Context, magnet string) (*Metadata, error) {
	// Parsing up front rejects malformed links without touching the DHT.
	if _, err := metainfo.ParseMagnetV2Uri(magnet); err != nil {
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

	// For v2/hybrid torrents we need to keep peer connections alive while
	// BEP 52 hash requests run. Disallow actual data transfer so this remains
	// a metadata-only operation.
	if info.HasV2() {
		t.DisallowDataDownload()
		t.DownloadAll()
	}
	mi := t.Metainfo()
	return f.metadataFromTorrent(ctx, t, mi, info)
}

// Repair attempts to fetch missing BEP 52 piece layers for an existing
// metainfo. It does not download torrent content. The returned metainfo is a
// copy with PieceLayers filled only when the layers are complete.
func (f *Fetcher) Repair(
	ctx context.Context,
	mi metainfo.MetaInfo,
	timeout time.Duration,
) (metainfo.MetaInfo, bool, string, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return mi, false, "", fmt.Errorf("parse info dictionary: %w", err)
	}
	if !info.HasV2() {
		return mi, true, "", nil
	}
	if err := metainfo.ValidatePieceLayers(mi.PieceLayers, &info.FileTree, info.PieceLength); err == nil {
		return mi, true, "", nil
	}

	repairCtx := ctx
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
		var cancel context.CancelFunc
		repairCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var t *torrent.Torrent
	waitInfo := &info
	// A v2-only torrent needs a magnet lookup to find peers that can provide
	// its piece layers. With timeout == 0 the caller asked for a single
	// immediate check, so use the local info instead of waiting for metadata.
	if info.HasV1() || timeout <= 0 {
		// Hybrid torrents already have v1 piece hashes, which lets us keep
		// peer connections alive without asking the network for metadata.
		spec, specErr := torrent.TorrentSpecFromMetaInfoErr(&mi)
		if specErr != nil {
			return mi, false, "", fmt.Errorf("build torrent spec: %w", specErr)
		}
		spec.DisallowDataDownload = true
		spec.DisallowDataUpload = true
		t, _, err = f.client.AddTorrentSpec(spec)
		if err != nil {
			return mi, false, "", fmt.Errorf("add metainfo: %w", err)
		}
	} else {
		// v2-only torrents have no piece hashes until layers arrive. A magnet
		// lookup gives the client a reason to connect and request them.
		magnet, magnetErr := mi.MagnetV2()
		if magnetErr != nil {
			return mi, false, "", fmt.Errorf("build magnet: %w", magnetErr)
		}
		t, err = f.client.AddMagnet(magnet.String())
		if err != nil {
			return mi, false, "", fmt.Errorf("add magnet: %w", err)
		}
		select {
		case <-t.GotInfo():
		case <-repairCtx.Done():
			t.Drop()
			if errors.Is(repairCtx.Err(), context.DeadlineExceeded) {
				return mi, false, "", &TimeoutError{Stage: "metadata lookup", Timeout: timeout}
			}
			return mi, false, "", repairCtx.Err()
		}
		waitInfo = t.Info()
		if waitInfo == nil {
			t.Drop()
			return mi, false, "", errors.New("torrent client reported metadata but info is nil")
		}
		fetched := t.Metainfo()
		if !bytes.Equal(fetched.InfoBytes, mi.InfoBytes) {
			t.Drop()
			return mi, false, "", errors.New("fetched metadata does not match the input torrent")
		}
	}
	defer t.Drop()

	// Keep connections active for BEP 52 hash requests, but never request
	// actual torrent data.
	t.DisallowDataDownload()
	t.DownloadAll()

	remaining := timeout
	if !deadline.IsZero() {
		remaining = time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
	}
	layers, complete, warning, err := f.waitPieceLayers(ctx, t, waitInfo, remaining)
	if err != nil {
		return mi, false, "", err
	}
	if !complete {
		return mi, false, warning, nil
	}
	mi.PieceLayers = layers
	return mi, true, "", nil
}

func (f *Fetcher) metadataFromTorrent(
	ctx context.Context,
	t *torrent.Torrent,
	mi metainfo.MetaInfo,
	info *metainfo.Info,
) (*Metadata, error) {
	v1, v2 := infoHashes(&mi, info)
	version := torrentVersion(info)
	primary := v1
	if primary == "" {
		primary = v2
	}

	complete := true
	warning := ""
	if info.HasV2() {
		layers, layersComplete, layersWarning, err := f.waitPieceLayers(
			ctx, t, info, f.config.PieceLayersTimeout,
		)
		if err != nil {
			return nil, err
		}
		complete = layersComplete
		warning = layersWarning
		if complete {
			mi.PieceLayers = layers
		} else {
			// A partial "piece layers" dictionary is invalid per BEP 52.
			mi.PieceLayers = nil
		}
	}

	buf := new(bytes.Buffer)
	if err := mi.Write(buf); err != nil {
		return nil, fmt.Errorf("encode metainfo: %w", err)
	}
	if buf.Len() == 0 {
		return nil, errors.New("encoded metainfo is empty")
	}

	if version == model.VersionV2 && !complete && !f.config.AllowIncompleteV2 {
		return nil, fmt.Errorf("%w: %s (use --allow-incomplete-v2 to save it)", ErrIncompleteV2, warning)
	}

	name := info.Name
	if name == "" {
		name = info.NameUtf8
	}
	f.log.Debug("metadata retrieved",
		"info_hash", primary,
		"info_hash_v1", v1,
		"info_hash_v2", v2,
		"version", version,
		"piece_layers_complete", complete,
		"name", name,
		"length", info.TotalLength(),
		"peers", len(t.PeerConns()),
	)

	return &Metadata{
		InfoHash:            primary,
		InfoHashV1:          v1,
		InfoHashV2:          v2,
		Version:             version,
		Name:                name,
		Length:              info.TotalLength(),
		Files:               len(info.UpvertedFiles()),
		PieceLayersComplete: complete,
		Warning:             warning,
		Torrent:             buf.Bytes(),
	}, nil
}

func (f *Fetcher) waitPieceLayers(
	ctx context.Context,
	t *torrent.Torrent,
	info *metainfo.Info,
	timeout time.Duration,
) (map[string]string, bool, string, error) {
	if !info.HasV2() {
		return nil, true, "", nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		mi := t.Metainfo()
		if err := metainfo.ValidatePieceLayers(mi.PieceLayers, &info.FileTree, info.PieceLength); err == nil {
			return mi.PieceLayers, true, "", nil
		} else {
			lastErr = err
		}
		if timeout <= 0 || time.Now().After(deadline) {
			break
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, "", ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = errors.New("piece layers are not available")
	}
	warning := "piece layers are incomplete: " + lastErr.Error()
	if timeout > 0 {
		warning = fmt.Sprintf("piece layers were not available after %s: %v", timeout, lastErr)
	}
	return nil, false, warning, nil
}

func infoHashes(mi *metainfo.MetaInfo, info *metainfo.Info) (v1, v2 string) {
	if info.HasV1() {
		v1 = mi.HashInfoBytes().HexString()
	}
	if info.HasV2() {
		h := infohash_v2.HashBytes(mi.InfoBytes)
		v2 = h.HexString()
	}
	return
}

func torrentVersion(info *metainfo.Info) string {
	switch {
	case info.HasV1() && info.HasV2():
		return model.VersionHybrid
	case info.HasV2():
		return model.VersionV2
	default:
		return model.VersionV1
	}
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
