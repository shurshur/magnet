package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLinksFromArgsAndFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "links.txt")
	if err := os.WriteFile(file, []byte("# comment\nmagnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n\nmagnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	links, err := readLinks([]string{
		"magnet:?xt=urn:btih:cccccccccccccccccccccccccccccccccccccccc",
		"@" + file,
	}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 3 {
		t.Fatalf("links = %v", links)
	}
	if links[1] != "magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected links: %v", links)
	}
}

func TestReadLinksFromStdin(t *testing.T) {
	links, err := readLinks([]string{"-"}, strings.NewReader("magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n# skip\nmagnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Fatalf("links = %v", links)
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	app := &App{Version: "test", Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}
	if code := app.Run([]string{"--version"}); code != 0 {
		t.Fatalf("version exit code = %d", code)
	}
	if !strings.Contains(out.String(), "magnet test") {
		t.Fatalf("version output = %q", out.String())
	}

	out.Reset()
	if code := app.Run([]string{"--help"}); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(out.String(), "fetch") || !strings.Contains(out.String(), "daemon") {
		t.Fatalf("help output does not describe modes: %q", out.String())
	}

	errOut.Reset()
	if code := app.Run([]string{"no-such-command"}); code != 2 {
		t.Fatalf("unknown command exit code = %d", code)
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatBytes(0); got != "-" {
		t.Fatalf("formatBytes(0) = %q", got)
	}
	if got := formatBytes(1536); got != "1.5 KiB" {
		t.Fatalf("formatBytes(1536) = %q", got)
	}
	if got := shortDuration(120 * time.Minute); got != "2h" {
		t.Fatalf("shortDuration(120m) = %q", got)
	}
}

func TestParseFlagsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	timeout := fs.Duration("timeout", 0, "")
	force := fs.Bool("force", false, "")

	link := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	ok, code := parseFlags(fs, []string{link, "--server", "http://127.0.0.1:4444", "-timeout", "5m", "--force"})
	if !ok || code != 0 {
		t.Fatalf("parseFlags returned ok=%v code=%d", ok, code)
	}
	if *server != "http://127.0.0.1:4444" {
		t.Fatalf("server = %q", *server)
	}
	if *timeout != 5*time.Minute {
		t.Fatalf("timeout = %s", *timeout)
	}
	if !*force {
		t.Fatal("force was not set")
	}
	if fs.NArg() != 1 || fs.Arg(0) != link {
		t.Fatalf("positionals = %v", fs.Args())
	}
}

func TestParseFlagsEqualsFormAndDashValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	ok, _ := parseFlags(fs, []string{"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "--server=http://host:1"})
	if !ok || *server != "http://host:1" {
		t.Fatalf("equals form: ok=%v server=%q", ok, *server)
	}

	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server = fs.String("server", "", "")
	ok, _ = parseFlags(fs, []string{"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "--server", "--looks-like-a-flag"})
	if !ok || *server != "--looks-like-a-flag" {
		t.Fatalf("dash value: ok=%v server=%q", ok, *server)
	}
}

func TestParseFlagsDoubleDashStopsOptionParsing(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	ok, _ := parseFlags(fs, []string{"magnet-a", "--", "--server", "value"})
	if !ok {
		t.Fatal("parse failed")
	}
	if *server != "" {
		t.Fatalf("server should not be parsed after --: %q", *server)
	}
	args := fs.Args()
	if len(args) != 3 || args[0] != "magnet-a" || args[1] != "--server" || args[2] != "value" {
		t.Fatalf("args = %v", args)
	}
}

func TestParseFlagsDoubleDashWithoutLeadingPositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "")
	ok, _ := parseFlags(fs, []string{"--", "-o", "file"})
	if !ok {
		t.Fatal("parse failed")
	}
	if *out != "" {
		t.Fatalf("out should not be parsed after --: %q", *out)
	}
	args := fs.Args()
	if len(args) != 2 || args[0] != "-o" || args[1] != "file" {
		t.Fatalf("args = %v", args)
	}
}
