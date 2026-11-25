// Command magnet-web runs a lightweight local web UI for magnet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shurshur/magnet/internal/appdirs"
	"github.com/shurshur/magnet/internal/webui"
)

// version is injected at build time.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("magnet-web", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(fs) }

	opts := webui.Options{}
	var showVersion bool
	var logLevel string
	fs.StringVar(&opts.Server, "server", env("MAGNET_SERVER"), "connect to a daemon at URL (default: auto-detect 127.0.0.1:8787)")
	fs.StringVar(&opts.APIToken, "api-token", env("MAGNET_API_TOKEN"), "daemon API token (env MAGNET_API_TOKEN)")
	fs.BoolVar(&opts.Embedded, "embedded", false, "always start the embedded engine; do not look for a daemon")
	fs.StringVar(&opts.Listen, "listen", "127.0.0.1:0", "local web UI listen address")
	fs.StringVar(&opts.DataDir, "data-dir", appdirs.DefaultDataDir(), "data directory for the embedded engine")
	fs.IntVar(&opts.Workers, "workers", 4, "number of concurrent searches")
	fs.DurationVar(&opts.AttemptTimeout, "attempt-timeout", 15*time.Minute, "timeout for one attempt (0 = unlimited)")
	fs.DurationVar(&opts.RetryDelay, "retry-delay", 30*time.Second, "delay before retrying")
	fs.DurationVar(&opts.PieceLayersTimeout, "piece-layers-timeout", 30*time.Second, "how long to wait for v2/hybrid piece layers (0 = do not wait)")
	fs.BoolVar(&opts.AllowIncompleteV2, "allow-incomplete-v2", false, "save v2 without piece layers with incomplete status")
	fs.IntVar(&opts.MaxAttempts, "max-attempts", 0, "maximum attempts per link (0 = unlimited)")
	fs.IntVar(&opts.Port, "port", 0, "UDP/TCP port for DHT and peer connections (0 = random)")
	fs.BoolVar(&opts.DisableTrackers, "no-trackers", false, "do not use trackers; DHT only")
	fs.BoolVar(&opts.NoBrowser, "no-browser", false, "do not open the browser automatically")
	fs.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		fmt.Printf("magnet-web %s\n", version)
		return 0
	}
	if opts.Server != "" && opts.Embedded {
		fmt.Fprintln(os.Stderr, "magnet-web: --server and --embedded cannot be used together")
		return 2
	}
	if opts.Workers < 1 {
		fmt.Fprintln(os.Stderr, "magnet-web: --workers must be greater than zero")
		return 2
	}
	if opts.AttemptTimeout < 0 || opts.RetryDelay < 0 || opts.PieceLayersTimeout < 0 || opts.MaxAttempts < 0 {
		fmt.Fprintln(os.Stderr, "magnet-web: timeouts and --max-attempts cannot be negative")
		return 2
	}
	if opts.Port < 0 || opts.Port > 65535 {
		fmt.Fprintln(os.Stderr, "magnet-web: --port must be in the range 0..65535")
		return 2
	}
	level, err := parseLogLevel(logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "magnet-web: %v\n", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	opts.Logger = log
	opts.Version = version
	opts.Out = os.Stdout

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := webui.Run(ctx, opts); err != nil {
		log.Error("magnet-web stopped with an error", "error", err)
		return 1
	}
	return 0
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `magnet-web — local web interface for magnet

Usage:
  magnet-web [options]

By default the web UI checks for a daemon at http://127.0.0.1:8787. If one is running,
it connects to it. Otherwise it starts the embedded engine.

Examples:
  magnet-web
  magnet-web --no-browser --listen 127.0.0.1:9090
  magnet-web --server http://127.0.0.1:8787 --api-token secret
  magnet-web --embedded --data-dir ~/.local/state/magnet-web

Options:
`)
	fs.PrintDefaults()
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

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
