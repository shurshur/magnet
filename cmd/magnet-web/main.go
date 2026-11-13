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
	fs.StringVar(&opts.Server, "server", env("MAGNET_SERVER"), "подключиться к демону по URL (по умолчанию — автоопределение 127.0.0.1:8787)")
	fs.StringVar(&opts.APIToken, "api-token", env("MAGNET_API_TOKEN"), "API-токен демона (env MAGNET_API_TOKEN)")
	fs.BoolVar(&opts.Embedded, "embedded", false, "всегда запускать встроенный движок, не искать демон")
	fs.StringVar(&opts.Listen, "listen", "127.0.0.1:0", "адрес локального web-интерфейса")
	fs.StringVar(&opts.DataDir, "data-dir", appdirs.DefaultDataDir(), "каталог данных встроенного движка")
	fs.IntVar(&opts.Workers, "workers", 4, "число одновременных поисков")
	fs.DurationVar(&opts.AttemptTimeout, "attempt-timeout", 15*time.Minute, "таймаут одной попытки (0 — без ограничения)")
	fs.DurationVar(&opts.RetryDelay, "retry-delay", 30*time.Second, "пауза перед повторной попыткой")
	fs.IntVar(&opts.MaxAttempts, "max-attempts", 0, "максимум попыток на ссылку (0 — без ограничения)")
	fs.IntVar(&opts.Port, "port", 0, "UDP/TCP-порт DHT и peer-соединений (0 — случайный)")
	fs.BoolVar(&opts.DisableTrackers, "no-trackers", false, "не использовать трекеры, только DHT")
	fs.BoolVar(&opts.NoBrowser, "no-browser", false, "не открывать браузер автоматически")
	fs.StringVar(&logLevel, "log-level", "info", "уровень лога: debug, info, warn, error")
	fs.BoolVar(&showVersion, "version", false, "показать версию и выйти")
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
		fmt.Fprintln(os.Stderr, "magnet-web: --server и --embedded нельзя использовать вместе")
		return 2
	}
	if opts.Workers < 1 {
		fmt.Fprintln(os.Stderr, "magnet-web: --workers должен быть больше нуля")
		return 2
	}
	if opts.AttemptTimeout < 0 || opts.RetryDelay < 0 || opts.MaxAttempts < 0 {
		fmt.Fprintln(os.Stderr, "magnet-web: таймауты и --max-attempts не могут быть отрицательными")
		return 2
	}
	if opts.Port < 0 || opts.Port > 65535 {
		fmt.Fprintln(os.Stderr, "magnet-web: --port должен быть в диапазоне 0..65535")
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
	fmt.Fprintf(fs.Output(), `magnet-web — локальный web-интерфейс для magnet

Использование:
  magnet-web [параметры]

По умолчанию web UI проверяет наличие демона на http://127.0.0.1:8787. Если он запущен,
интерфейс подключается к нему. Иначе запускается встроенный движок.

Примеры:
  magnet-web
  magnet-web --no-browser --listen 127.0.0.1:9090
  magnet-web --server http://127.0.0.1:8787 --api-token secret
  magnet-web --embedded --data-dir ~/.local/state/magnet-web

Параметры:
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
		return 0, fmt.Errorf("неизвестный --log-level %q", s)
	}
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
