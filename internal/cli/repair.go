package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/model"
)

type repairResult struct {
	Input   string `json:"input"`
	Output  string `json:"output,omitempty"`
	Version string `json:"version,omitempty"`
	State   string `json:"state,omitempty"`
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (a *App) runRepair(args []string) int {
	fs := newFlagSet("repair", a.Stderr)
	out := stringFlag(fs, "out", "o", "", "directory or path for the repaired .torrent (default: <file>.repaired.torrent)")
	timeout := durationFlag(fs, "timeout", "t", 30*time.Second, "how long to wait for piece layers (0 = check once)")
	force := boolFlag(fs, "force", "f", false, "overwrite an existing file")
	port := intFlag(fs, "port", "", 0, "UDP/TCP port for DHT and peer connections (0 = random)")
	noTrackers := boolFlag(fs, "no-trackers", "", false, "do not use trackers; DHT only")
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print progress")
	debug := boolFlag(fs, "debug", "", false, "verbose torrent client log")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *timeout < 0 {
		fmt.Fprintln(a.Stderr, "magnet repair: --timeout cannot be negative")
		return 2
	}
	files := fs.Args()
	if len(files) == 0 {
		fmt.Fprintln(a.Stderr, "magnet repair: provide at least one .torrent file")
		return 2
	}
	if len(files) > 1 && *out != "" && strings.HasSuffix(strings.ToLower(*out), ".torrent") {
		fmt.Fprintln(a.Stderr, "magnet repair: --out must be a directory for multiple files")
		return 2
	}

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: level}))
	cacheDir, err := os.MkdirTemp("", "magnet-repair-*")
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet repair: create temporary directory: %v\n", err)
		return 1
	}
	defer os.RemoveAll(cacheDir)

	fetcher, err := metafetch.New(metafetch.Config{
		DataDir:            cacheDir,
		ListenPort:         *port,
		DisableTrackers:    *noTrackers,
		PieceLayersTimeout: *timeout,
		AllowIncompleteV2:  true,
		Logger:             log,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet repair: %v\n", err)
		return 1
	}
	defer func() {
		if err := fetcher.Close(); err != nil {
			log.Debug("torrent client shutdown", "error", err)
		}
	}()

	baseCtx, stop := a.signalContext()
	defer stop()

	results := make([]repairResult, 0, len(files))
	hadError := false
	for i, input := range files {
		if baseCtx.Err() != nil {
			fmt.Fprintln(a.Stderr, "magnet repair: interrupted")
			if *jsonOut {
				printJSON(a.Stdout, results)
			}
			return 130
		}
		res := repairResult{Input: input}
		mi, info, loadErr := loadTorrentFile(input)
		if loadErr != nil {
			res.Error = loadErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(files), input, loadErr)
			continue
		}
		res.Version = torrentVersion(info)
		if !info.HasV2() {
			res.State = string(model.StateFound)
			results = append(results, res)
			if !*quiet {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: v1, piece layers are not required\n", i+1, len(files), input)
			}
			continue
		}
		if err := metainfo.ValidatePieceLayers(mi.PieceLayers, &info.FileTree, info.PieceLength); err == nil {
			res.State = string(model.StateFound)
			results = append(results, res)
			if !*quiet {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: piece layers are already complete\n", i+1, len(files), input)
			}
			continue
		}

		if !*quiet {
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: requesting piece layers from v2 peers...\n", i+1, len(files), input)
		}
		stopProgress := startProgress(baseCtx, a.Stderr, filepath.Base(input), *quiet)
		repaired, complete, warning, repairErr := fetcher.Repair(baseCtx, *mi, *timeout)
		stopProgress()
		if repairErr != nil {
			if baseCtx.Err() != nil {
				if *jsonOut {
					printJSON(a.Stdout, results)
				}
				return 130
			}
			res.Error = repairErrorText(repairErr, *timeout)
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %s\n", i+1, len(files), input, res.Error)
			continue
		}
		if !complete {
			res.State = string(model.StateIncomplete)
			res.Warning = warning
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %s\n", i+1, len(files), input, warning)
			continue
		}
		if err := metainfo.ValidatePieceLayers(repaired.PieceLayers, &info.FileTree, info.PieceLength); err != nil {
			res.Error = "repair returned invalid piece layers: " + err.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %s\n", i+1, len(files), input, res.Error)
			continue
		}

		output, outErr := repairOutputPath(input, *out, len(files), *force)
		if outErr != nil {
			res.Error = outErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(files), input, outErr)
			continue
		}
		data, marshalErr := marshalMetaInfo(repaired)
		if marshalErr != nil {
			res.Error = marshalErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(files), input, marshalErr)
			continue
		}
		if err := fsutil.AtomicWriteFile(output, data, 0o644); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: %v\n", i+1, len(files), input, err)
			continue
		}
		res.Output = output
		res.State = string(model.StateFound)
		results = append(results, res)
		if !*jsonOut {
			fmt.Fprintln(a.Stdout, output)
		}
		if !*quiet {
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: saved: %s\n", i+1, len(files), input, output)
		}
	}

	if *jsonOut {
		printJSON(a.Stdout, results)
	}
	if hadError {
		return 1
	}
	return 0
}

func repairErrorText(err error, timeout time.Duration) string {
	var timeoutErr *metafetch.TimeoutError
	if errors.As(err, &timeoutErr) {
		if timeoutErr.Stage == "metadata lookup" {
			return fmt.Sprintf(
				"could not fetch metadata from v2 peers within %s; check that peers are available or increase --timeout",
				timeoutErr.Timeout,
			)
		}
		return fmt.Sprintf(
			"could not fetch piece layers within %s; v2 peers did not respond in time; the input file was not modified",
			timeoutErr.Timeout,
		)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf(
			"could not fetch piece layers within %s; v2 peers did not respond in time; the input file was not modified",
			timeout,
		)
	}
	return err.Error()
}

func loadTorrentFile(path string) (*metainfo.MetaInfo, *metainfo.Info, error) {
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, nil, fmt.Errorf("parse info in %s: %w", path, err)
	}
	return mi, &info, nil
}

func marshalMetaInfo(mi metainfo.MetaInfo) ([]byte, error) {
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return nil, fmt.Errorf("encode metainfo: %w", err)
	}
	return buf.Bytes(), nil
}

func repairOutputPath(input, out string, count int, force bool) (string, error) {
	base := filepath.Base(input)
	if !strings.HasSuffix(strings.ToLower(base), ".torrent") {
		base += ".torrent"
	}
	stem := strings.TrimSuffix(base, ".torrent") + ".repaired.torrent"
	var path string
	if out == "" {
		path = filepath.Join(filepath.Dir(input), stem)
	} else if count > 1 || strings.HasSuffix(out, string(os.PathSeparator)) || isDir(out) {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", err
		}
		path = filepath.Join(out, stem)
	} else {
		path = out
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("output file %s already exists (use --force)", path)
		}
	}
	return path, nil
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
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
