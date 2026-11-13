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
	listen := stringFlag(fs, "listen", "", "127.0.0.1:8787", "адрес HTTP API")
	dataDir := stringFlag(fs, "data-dir", "", appdirs.DefaultDataDir(), "каталог состояния, БД и .torrent-файлов")
	apiToken := stringFlag(fs, "api-token", "", defaultToken(), "токен API (env MAGNET_API_TOKEN)")
	workers := intFlag(fs, "workers", "", 4, "число одновременных поисков")
	attemptTimeout := durationFlag(fs, "attempt-timeout", "", 15*time.Minute, "таймаут одной попытки поиска (0 — без ограничения)")
	retryDelay := durationFlag(fs, "retry-delay", "", 30*time.Second, "пауза перед повторной попыткой")
	maxAttempts := intFlag(fs, "max-attempts", "", 0, "максимум попыток на ссылку (0 — без ограничения)")
	port := intFlag(fs, "port", "", 0, "UDP/TCP-порт DHT и peer-соединений (0 — случайный)")
	noTrackers := boolFlag(fs, "no-trackers", "", false, "не использовать трекеры, только DHT")
	logLevel := stringFlag(fs, "log-level", "", "info", "уровень лога: debug, info, warn, error")
	logFormat := stringFlag(fs, "log-format", "", "text", "формат лога: text или json")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *workers < 1 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --workers должен быть больше нуля")
		return 2
	}
	if *attemptTimeout < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --attempt-timeout не может быть отрицательным")
		return 2
	}
	if *retryDelay < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --retry-delay не может быть отрицательным")
		return 2
	}
	if *maxAttempts < 0 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --max-attempts не может быть отрицательным")
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintln(a.Stderr, "magnet daemon: --port должен быть в диапазоне 0..65535")
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
		fmt.Fprintf(a.Stderr, "magnet daemon: путь к данным: %v\n", err)
		return 1
	}
	torrentDir := filepath.Join(root, "torrents")
	cacheDir := filepath.Join(root, "cache")
	for _, dir := range []string{root, torrentDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(a.Stderr, "magnet daemon: создать каталог %s: %v\n", dir, err)
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
		DataDir:         cacheDir,
		ListenPort:      *port,
		DisableTrackers: *noTrackers,
		Logger:          log,
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
		fmt.Fprintf(a.Stderr, "magnet daemon: слушать %s: %v\n", *listen, err)
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
		return 0, fmt.Errorf("неизвестный --log-level %q", s)
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
		return nil, fmt.Errorf("неизвестный --log-format %q", format)
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
