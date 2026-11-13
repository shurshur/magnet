// Package cli implements the magnet command-line interface.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// App carries the streams used by the CLI. It exists mainly to keep commands
// testable.
type App struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run executes the CLI and returns a process exit code.
func Run(args []string, version string) int {
	app := &App{Version: version, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(args)
}

// Run executes the CLI with the app's streams.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.printUsage(a.Stdout)
		return 2
	}
	cmd := args[0]
	args = args[1:]
	switch cmd {
	case "-h", "--help", "help":
		a.printUsage(a.Stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Fprintf(a.Stdout, "magnet %s\n", a.Version)
		return 0
	case "fetch", "get-torrent":
		return a.runFetch(args)
	case "daemon", "serve":
		return a.runDaemon(args)
	case "client":
		if len(args) == 0 {
			a.printUsage(a.Stdout)
			return 2
		}
		return a.runClient(args[0], args[1:])
	case "add":
		return a.runClient("add", args)
	case "ls", "list":
		return a.runClient("ls", args)
	case "show", "status":
		return a.runClient("show", args)
	case "wait":
		return a.runClient("wait", args)
	case "get", "download":
		return a.runClient("get", args)
	case "cancel":
		return a.runClient("cancel", args)
	case "retry":
		return a.runClient("retry", args)
	case "rm", "remove", "delete":
		return a.runClient("rm", args)
	case "stats":
		return a.runClient("stats", args)
	case "ping", "health":
		return a.runClient("ping", args)
	default:
		fmt.Fprintf(a.Stderr, "magnet: неизвестная команда %q\n\n", cmd)
		a.printUsage(a.Stderr)
		return 2
	}
}

func (a *App) printUsage(w io.Writer) {
	fmt.Fprint(w, `magnet — поиск .torrent-файлов в DHT по magnet-ссылкам

Использование:
  magnet <команда> [параметры]

Разовый режим:
  fetch <magnet|@file|->...   найти метаданные и сохранить .torrent
                              (несколько ссылок, файлы @file и stdin через -)

Демон:
  daemon                      запустить долгоживущий демон с HTTP API

Клиент демона (те же команды доступны как "magnet client <команда>"):
  add <magnet|@file|->...     поставить ссылки в очередь
  ls                          список заданий
  show <id>...                подробности задания
  wait <id>...                ждать завершения и вывести пути
  get <id>...                 скачать готовые .torrent-файлы
  cancel <id>...              отменить задания
  retry <id>...               повторить неудачные задания
  rm <id>...                  убрать задания из демона (--purge — с файлами)
  stats                       счётчики демона
  ping                        проверить доступность демона

Общие параметры клиента:
  --server URL                адрес демона (env MAGNET_SERVER,
                              по умолчанию http://127.0.0.1:8787)
  --token TOKEN               API-токен (env MAGNET_API_TOKEN)

Примеры:
  magnet fetch 'magnet:?xt=urn:btih:...'
  cat links.txt | magnet fetch -o downloads/ -
  magnet daemon --data-dir ~/.local/state/magnet
  magnet add 'magnet:?xt=urn:btih:...'
  magnet wait 0123456789abcdef0123456789abcdef01234567
  magnet get -o downloads/ 0123456789abcdef0123456789abcdef01234567

Запустите "magnet <команда> -h" для параметров конкретной команды.
`)
}

func (a *App) signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

func stringFlag(fs *flag.FlagSet, name, short, def, usage string) *string {
	p := fs.String(name, def, usage)
	if short != "" {
		fs.StringVar(p, short, def, usage)
	}
	return p
}

func boolFlag(fs *flag.FlagSet, name, short string, def bool, usage string) *bool {
	p := fs.Bool(name, def, usage)
	if short != "" {
		fs.BoolVar(p, short, def, usage)
	}
	return p
}

func durationFlag(fs *flag.FlagSet, name, short string, def time.Duration, usage string) *time.Duration {
	p := fs.Duration(name, def, usage)
	if short != "" {
		fs.DurationVar(p, short, def, usage)
	}
	return p
}

func intFlag(fs *flag.FlagSet, name, short string, def int, usage string) *int {
	p := fs.Int(name, def, usage)
	if short != "" {
		fs.IntVar(p, short, def, usage)
	}
	return p
}

func parseFlags(fs *flag.FlagSet, args []string) (bool, int) {
	if err := fs.Parse(interspersedArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0
		}
		return false, 2
	}
	return true, 0
}

// interspersedArgs makes Go's flag package accept flags after positional
// arguments. The standard parser stops at the first non-flag, which is
// surprising for commands like:
//
//	magnet add "magnet:?..." --server http://127.0.0.1:4444
//
// Flags are moved before positionals while preserving their values. Everything
// after a standalone "--" is treated as positional.
func interspersedArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		name := strings.TrimLeft(arg, "-")
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
			hasValue = true
		}
		f := fs.Lookup(name)
		flags = append(flags, arg)
		if f == nil || hasValue || isBoolFlag(f) {
			continue
		}
		// A non-boolean flag consumes the following argument, even if it
		// starts with a dash.
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	// If the first positional begins with a dash (possible after "--"), keep
	// a separator so flag.Parse does not try to interpret it as a flag.
	if len(positional) > 0 && positional[0] != "-" && strings.HasPrefix(positional[0], "-") {
		flags = append(flags, "--")
	}
	return append(flags, positional...)
}

func isBoolFlag(f *flag.Flag) bool {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return b.IsBoolFlag()
	}
	return false
}

// readLinks expands arguments into magnet links. Supported forms:
//   - a literal magnet link
//   - @path — read links from a file (one per line)
//   - - — read links from stdin
//
// With no arguments, stdin is read when it is not a terminal.
func readLinks(args []string, stdin io.Reader) ([]string, error) {
	var links []string
	readAll := func(r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			links = append(links, line)
		}
		return sc.Err()
	}

	if len(args) == 0 {
		if stdinIsTerminal(stdin) {
			return nil, errors.New("не задано ни одной magnet-ссылки")
		}
		if err := readAll(stdin); err != nil {
			return nil, fmt.Errorf("чтение stdin: %w", err)
		}
		if len(links) == 0 {
			return nil, errors.New("на stdin нет magnet-ссылок")
		}
		return links, nil
	}

	for _, arg := range args {
		switch {
		case arg == "-":
			if err := readAll(stdin); err != nil {
				return nil, fmt.Errorf("чтение stdin: %w", err)
			}
		case strings.HasPrefix(arg, "@"):
			path := strings.TrimPrefix(arg, "@")
			if path == "" {
				return nil, errors.New("пустое имя файла после @")
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("открытие %s: %w", path, err)
			}
			err = readAll(f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("чтение %s: %w", path, err)
			}
		default:
			links = append(links, strings.TrimSpace(arg))
		}
	}
	if len(links) == 0 {
		return nil, errors.New("не найдено ни одной magnet-ссылки")
	}
	return links, nil
}

func stdinIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func defaultServer() string {
	if v := strings.TrimSpace(os.Getenv("MAGNET_SERVER")); v != "" {
		return v
	}
	return "http://127.0.0.1:8787"
}

func defaultToken() string {
	return strings.TrimSpace(os.Getenv("MAGNET_API_TOKEN"))
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
