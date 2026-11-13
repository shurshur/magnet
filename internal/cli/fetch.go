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
)

type fetchResult struct {
	Link     string `json:"link"`
	InfoHash string `json:"info_hash,omitempty"`
	Name     string `json:"name,omitempty"`
	Path     string `json:"path,omitempty"`
	Length   int64  `json:"length,omitempty"`
	Files    int    `json:"files,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (a *App) runFetch(args []string) int {
	fs := newFlagSet("fetch", a.Stderr)
	out := stringFlag(fs, "out", "o", ".", "каталог или путь к .torrent-файлу")
	timeout := durationFlag(fs, "timeout", "t", 15*time.Minute, "таймаут поиска одной ссылки (0 — без ограничения)")
	force := boolFlag(fs, "force", "f", false, "перезаписать существующий .torrent")
	port := intFlag(fs, "port", "", 0, "UDP/TCP-порт DHT и peer-соединений (0 — случайный)")
	noTrackers := boolFlag(fs, "no-trackers", "", false, "не использовать трекеры, только DHT")
	jsonOut := boolFlag(fs, "json", "", false, "вывести результат в JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "не выводить прогресс")
	debug := boolFlag(fs, "debug", "", false, "подробный лог torrent-клиента")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *timeout < 0 {
		fmt.Fprintln(a.Stderr, "magnet fetch: таймаут не может быть отрицательным")
		return 2
	}

	links, err := readLinks(fs.Args(), a.Stdin)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet fetch: %v\n", err)
		return 2
	}
	if len(links) > 1 && strings.HasSuffix(strings.ToLower(*out), ".torrent") {
		fmt.Fprintln(a.Stderr, "magnet fetch: для нескольких ссылок --out должен быть каталогом")
		return 2
	}

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: level}))

	cacheDir, err := os.MkdirTemp("", "magnet-fetch-*")
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet fetch: создать временный каталог: %v\n", err)
		return 1
	}
	defer os.RemoveAll(cacheDir)

	fetcher, err := metafetch.New(metafetch.Config{
		DataDir:         cacheDir,
		ListenPort:      *port,
		DisableTrackers: *noTrackers,
		Logger:          log,
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
		id, _, parseErr := magnetutil.Parse(link)
		if parseErr != nil {
			res.Error = parseErr.Error()
			results = append(results, res)
			hadError = true
			fmt.Fprintf(a.Stderr, "[%d/%d] ошибка: %v\n", i+1, len(links), parseErr)
			continue
		}
		res.InfoHash = id
		shortID := id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		if !*quiet {
			fmt.Fprintf(a.Stderr, "[%d/%d] %s: поиск метаданных в DHT...\n", i+1, len(links), shortID)
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
				fetchErr = fmt.Errorf("метаданные не найдены за %s", timeout.String())
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
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: сохранить %s: %v\n", i+1, len(links), shortID, path, err)
				continue
			}
		}
		res.Path = path
		results = append(results, res)
		if !*quiet {
			if exists {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: файл уже существует: %s\n", i+1, len(links), shortID, path)
			} else {
				fmt.Fprintf(a.Stderr, "[%d/%d] %s: сохранено: %s\n", i+1, len(links), shortID, path)
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
		fmt.Fprintln(a.Stderr, "magnet fetch: прервано")
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
				fmt.Fprintf(w, "    %s: всё ещё ищем (%s)...\n", label, shortDuration(time.Since(start)))
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
