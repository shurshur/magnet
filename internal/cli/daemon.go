package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shurshur/magnet/internal/api"
	"github.com/shurshur/magnet/internal/appdirs"
	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/metafetch"
	"github.com/shurshur/magnet/internal/store"
)

func (a *App) runDaemon(args []string) int {
	fs := newFlagSet("daemon", a.Stderr)
	listen := stringFlag(fs, "listen", "", "127.0.0.1:8787", "HTTP API listen address")
	dataDir := stringFlag(fs, "data-dir", "", appdirs.DefaultDataDir(), "state, database, and .torrent files directory")
	apiToken := stringFlag(fs, "api-token", "", defaultToken(), "API token (env MAGNET_API_TOKEN)")
	workers := intFlag(fs, "workers", "", 4, "number of concurrent searches")
	attemptTimeout := durationFlag(fs, "attempt-timeout", "", 15*time.Minute, "timeout for one search attempt (0 = unlimited)")
	retryDelay := durationFlag(fs, "retry-delay", "", 30*time.Second, "delay before retrying")
	maxAttempts := intFlag(fs, "max-attempts", "", 0, "maximum attempts per link (0 = unlimited)")
	pieceTimeout := durationFlag(fs, "piece-layers-timeout", "", 30*time.Second, "how long to wait for v2/hybrid piece layers (0 = do not wait)")
	allowIncompleteV2 := boolFlag(fs, "allow-incomplete-v2", "", false, "save v2 without piece layers with incomplete status")
	port := intFlag(fs, "port", "", 0, "UDP/TCP port for DHT and peer connections (0 = random)")
	noTrackers := boolFlag(fs, "no-trackers", "", false, "do not use trackers; DHT only")
	logLevel := stringFlag(fs, "log-level", "", "info", "log level: debug, info, warn, error")
	logFormat := stringFlag(fs, "log-format", "", "text", "log format: text or json")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *workers < 1 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --workers must be greater than zero")
		return 2
	}
	if *attemptTimeout < 0 || *pieceTimeout < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: timeouts cannot be negative")
		return 2
	}
	if *retryDelay < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --retry-delay cannot be negative")
		return 2
	}
	if *maxAttempts < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --max-attempts cannot be negative")
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --port must be in the range 0..65535")
		return 2
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 2
	}
	log, err := newLogger(a.Stderr, *logFormat, level)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 2
	}

	root, err := filepath.Abs(*dataDir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: data path: %v\n", err)
		return 1
	}
	torrentDir := filepath.Join(root, "torrents")
	cacheDir := filepath.Join(root, "cache")
	for _, dir := range []string{root, torrentDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(a.Stderr, "magnet daemon: create directory %s: %v\n", dir, err)
			return 1
		}
	}

	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 1
	}
	defer st.Close()

	fetcher, err := metafetch.New(metafetch.Config{
		DataDir:            cacheDir,
		ListenPort:         *port,
		DisableTrackers:    *noTrackers,
		PieceLayersTimeout: *pieceTimeout,
		AllowIncompleteV2:  *allowIncompleteV2,
		Logger:             log,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 1
	}
	defer func() {
		if err := fetcher.Close(); err != nil {
			log.Debug("torrent client shutdown", "error", err)
		}
	}()

	mgr, err := daemon.New(daemon.Config{
		Store:          st,
		Fetcher:        fetcher,
		OutputDir:      torrentDir,
		Workers:        *workers,
		AttemptTimeout: *attemptTimeout,
		RetryDelay:     *retryDelay,
		MaxAttempts:    *maxAttempts,
		Logger:         log,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 1
	}

	ctx, stop := a.signalContext()
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		return 1
	}

	srvAPI, err := api.New(api.Config{
		Manager: mgr,
		Token:   strings.TrimSpace(*apiToken),
		Version: a.Version,
		Logger:  log,
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: %v\n", err)
		mgr.Close()
		return 1
	}
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srvAPI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet daemon: listen on %s: %v\n", *listen, err)
		mgr.Close()
		return 1
	}

	log.Info("magnet daemon started",
		"listen", ln.Addr().String(),
		"data_dir", root,
		"torrents_dir", torrentDir,
		"workers", *workers,
	)
	if strings.TrimSpace(*apiToken) == "" && !isLoopbackListen(ln.Addr().String()) {
		log.Warn("API listens on a non-loopback address without --api-token; anyone who can reach it can submit links")
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()

	exitCode := 0
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			exitCode = 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn("http server shutdown", "error", err)
	}
	mgr.Close()
	log.Info("magnet daemon stopped")
	return exitCode
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown --log-level %q", s)
	}
}

func newLogger(w io.Writer, format string, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text", "":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown --log-format %q", format)
	}
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
