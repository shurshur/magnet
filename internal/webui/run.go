package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/shurshur/magnet/internal/appdirs"
)

// Options controls the web UI process.
type Options struct {
	Server          string
	APIToken        string
	Embedded        bool
	Listen          string
	DataDir         string
	Workers         int
	AttemptTimeout  time.Duration
	RetryDelay      time.Duration
	MaxAttempts     int
	Port            int
	DisableTrackers bool
	NoBrowser       bool
	Version         string
	Logger          *slog.Logger
	Out             io.Writer
}

// Run starts the web UI HTTP server and blocks until ctx is canceled or the
// server fails. If Server is empty it first probes the default daemon and
// falls back to an in-process engine.
func Run(ctx context.Context, opts Options) error {
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.DataDir == "" {
		opts.DataDir = appdirs.DefaultDataDir()
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	log := opts.Logger

	backend, err := OpenBackend(ctx, opts, log)
	if err != nil {
		return err
	}
	defer backend.Close()

	srv, err := NewServer(backend, opts.Version, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Listen, err)
	}
	url := listenerURL(ln)
	if !isLoopbackListen(opts.Listen) {
		log.Warn("web UI listens on a non-loopback address without authentication; anyone who can reach it can control torrents")
	}
	log.Info("magnet web UI started",
		"url", url,
		"mode", backend.Info().Mode,
		"data_dir", backend.Info().DataDir,
		"server", backend.Info().Server,
	)
	fmt.Fprintln(opts.Out, url)

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()

	if !opts.NoBrowser {
		if err := openBrowser(url); err != nil {
			log.Warn("could not open the browser; open the URL manually", "url", url, "error", err)
		}
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http server shutdown", "error", err)
		_ = httpServer.Close()
	}
	return runErr
}

func listenerURL(ln net.Listener) string {
	addr := ln.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("opening a browser is not supported on %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}
