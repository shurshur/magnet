package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/magnetutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
)

type fetchResult struct {
	Link       string `json:"link"`
	InfoHash   string `json:"info_hash,omitempty"`
	InfoHashV1 string `json:"info_hash_v1,omitempty"`
	InfoHashV2 string `json:"info_hash_v2,omitempty"`
	Version    string `json:"version,omitempty"`
	State      string `json:"state,omitempty"`
	Name       string `json:"name,omitempty"`
	Path       string `json:"path,omitempty"`
	Length     int64  `json:"length,omitempty"`
	Files      int    `json:"files,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (a *App) runFetch(args []string) int {
	fs := newFlagSet("fetch", a.Stderr)
	out := stringFlag(fs, "out", "o", ".", "directory or .torrent file path")
	timeout := durationFlag(fs, "timeout", "t", 15*time.Minute, "per-link metadata search timeout (0 = unlimited)")
	pieceTimeout := durationFlag(fs, "piece-layers-timeout", "", 30*time.Second, "how long to wait for v2/hybrid piece layers (0 = do not wait)")
	allowIncompleteV2 := boolFlag(fs, "allow-incomplete-v2", "", false, "save v2 without piece layers with incomplete status")
	strict := boolFlag(fs, "strict", "", false, "treat incomplete as an error (exit code 1)")
	force := boolFlag(fs, "force", "f", false, "overwrite an existing .torrent")
	port := intFlag(fs, "port", "", 0, "UDP/TCP port for DHT and peer connections (0 = random)")
	noTrackers := boolFlag(fs, "no-trackers", "", false, "do not use trackers; DHT only")
	jsonOut := boolFlag(fs, "json", "", false, "output results as JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print progress")
	debug := boolFlag(fs, "debug", "", false, "verbose torrent client log")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *timeout < 0 || *pieceTimeout < 0 {
		fmt.Fprintln(a.Stderr, "magnet fetch: timeouts cannot be negative")
		return 2
	}

	links, err := readLinks(fs.Args(), a.Stdin)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet fetch: %v\n", err)
		return 2
	}
	if len(links) > 1 && strings.HasSuffix(strings.ToLower(*out), ".torrent") {
		fmt.Fprintln(a.Stderr, "magnet fetch: --out must be a directory when multiple links are given")
		return 2
	}

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: level}))

	cacheDir, err := os.MkdirTemp("", "magnet-fetch-*")
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet fetch: create temporary directory: %v\n", err)
		return 1
	}
	defer os.RemoveAll(cacheDir)

	fetcher, err := metafetch.New(metafetch.Config{
		DataDir:            cacheDir,
		ListenPort:         *port,
		DisableTrackers:    *noTrackers,
		PieceLayersTimeout: *pieceTimeout,
		AllowIncompleteV2:  *allowIncompleteV2,
		Logger:             log,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet fetch: %v\n", err)
		return 1
	}
	defer func() {
		if err := fetcher.Close(); err != nil {
			log.Debug("torrent client shutdown", "error", err)
		}
	}()

	baseCtx, stop := a.signalContext()
	defer stop()

	results := make([]fetchResult, 0, len(links))
	hadError := false
	interrupted := false

	for i, link := range links {
		if baseCtx.Err() != nil {
			interrupted = true
			break
		}
		res := fetchResult{Link: link}
		parsed, parseErr := magnetutil.Parse(link)
		if parseErr != nil {
			res.Error = parseErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] error: %v\n", i+1, len(links), parseErr)
			continue
		}
		id := parsed.PrimaryID
		res.InfoHash = id
		res.InfoHashV1 = parsed.InfoHashV1
		res.InfoHashV2 = parsed.InfoHashV2
		res.Version = parsed.Version
		shortID := id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		if !*quiet {
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: searching for metadata in the DHT...\n", i+1, len(links), shortID)
		}

		ctx := baseCtx
		var cancel context.CancelFunc = func() {}
		if *timeout > 0 {
			ctx, cancel = context.WithTimeout(baseCtx, *timeout)
		}
		stopProgress := startProgress(ctx, a.Stderr, shortID, *quiet)
		md, fetchErr := fetcher.Fetch(ctx, link)
		stopProgress()
		cancel()

		if fetchErr != nil {
			if baseCtx.Err() != nil {
				interrupted = true
				break
			}
			if *timeout > 0 && errors.Is(fetchErr, context.DeadlineExceeded) {
				fetchErr = fmt.Errorf("metadata not found within %s", timeout.String())
			}
			res.Error = fetchErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(links), shortID, fetchErr)
			continue
		}

		res.Name = md.Name
		res.Length = md.Length
		res.Files = md.Files
		res.InfoHashV1 = md.InfoHashV1
		res.InfoHashV2 = md.InfoHashV2
		res.Version = md.Version
		res.Warning = md.Warning
		res.State = string(model.StateFound)
		if md.Version != model.VersionV1 && !md.PieceLayersComplete {
			res.State = string(model.StateIncomplete)
			if *strict {
				hadError = true
			}
		}
		path, exists, saveErr := fsutil.ResolveOutputPath(*out, md.Name, md.InfoHash, *force)
		if saveErr != nil {
			res.Error = saveErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(links), shortID, saveErr)
			continue
		}
		if !exists {
			if err := fsutil.AtomicWriteFile(path, md.Torrent, 0o644); err != nil {
				res.Error = err.Error()
				results = append(results, res)
				hadError = true
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: save %s: %v\n", i+1, len(links), shortID, path, err)
				continue
			}
		}
		res.Path = path
		results = append(results, res)
		if res.Warning != "" {
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %s\n", i+1, len(links), shortID, res.Warning)
		}
		if !*quiet {
			if exists {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: file already exists: %s\n", i+1, len(links), shortID, path)
			} else {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: saved: %s\n", i+1, len(links), shortID, path)
			}
		}
		if !*jsonOut {
			fmt.Fprintln(a.Stdout, path)
		}
	}

	if *jsonOut {
		printJSON(a.Stdout, results)
	}

	if interrupted {
		fmt.Fprintln(a.Stderr, "magnet fetch: interrupted")
		return 130
	}
	if hadError {
		return 1
	}
	return 0
}

func startProgress(ctx context.Context, w io.Writer, label string, quiet bool) func() {
	if quiet {
		return func() {}
	}
	done := make(chan struct{})
	var once bool
	stop := func() {
		if !once {
			once = true
			close(done)
		}
	}
	go func() {
		start := time.Now()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintf(w, "    %s: still searching (%s)...\n", label, shortDuration(time.Since(start)))
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return stop
}

func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	h := fmt.Sprintf("%.1f", d.Hours())
	h = strings.TrimSuffix(h, ".0")
	return h + "h"
}
