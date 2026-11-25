package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shurshur/magnet/internal/api"
	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/magnetutil"
	"github.com/shurshur/magnet/internal/model"
)

func (a *App) runClient(cmd string, args []string) int {
	switch cmd {
	case "-h", "--help", "help":
		a.printUsage(a.Stdout)
		return 0
	case "add":
		return a.runClientAdd(args)
	case "ls", "list":
		return a.runClientList(args)
	case "show", "status":
		return a.runClientShow(args)
	case "wait":
		return a.runClientWait(args)
	case "get", "download":
		return a.runClientGet(args)
	case "cancel":
		return a.runClientCancel(args)
	case "retry":
		return a.runClientRetry(args)
	case "rm", "remove", "delete":
		return a.runClientRemove(args)
	case "stats":
		return a.runClientStats(args)
	case "ping", "health":
		return a.runClientPing(args)
	default:
		fmt.Fprintf(a.Stderr, "magnet client: unknown command %q\n", cmd)
		return 2
	}
}

func clientConnFlags(fs *flag.FlagSet) (*string, *string) {
	server := stringFlag(fs, "server", "s", defaultServer(), "daemon URL")
	token := stringFlag(fs, "token", "", defaultToken(), "API token")
	return server, token
}

func (a *App) runClientAdd(args []string) int {
	fs := newFlagSet("add", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "print only IDs")
	batchSize := intFlag(fs, "batch-size", "", 500, "batch size for bulk add")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *batchSize < 1 {
		fmt.Fprintln(a.Stderr, "magnet add: --batch-size must be greater than zero")
		return 2
	}
	links, err := readLinks(fs.Args(), a.Stdin)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet add: %v\n", err)
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)

	var all = make([]model.Job, 0, len(links))
	var failures = make([]map[string]string, 0)
	created, existing := 0, 0
	hadError := false

	// Validate locally first: a single malformed line must not force hundreds
	// of individual API requests or block the rest of a bulk import.
	validLinks := make([]string, 0, len(links))
	for _, link := range links {
		if _, err := magnetutil.Parse(link); err != nil {
			hadError = true
			failures = append(failures, map[string]string{"link": link, "error": err.Error()})
			fmt.Fprintf(a.Stderr, "%s: %v\n", link, err)
			continue
		}
		validLinks = append(validLinks, link)
	}

	addBatch := func(batch []string) bool {
		res, err := client.Add(ctx, batch)
		if err == nil {
			all = append(all, res.Jobs...)
			created += res.Created
			existing += res.Existing
			return true
		}
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest && len(batch) > 1 {
			// One bad link must not block a large bulk import. Retry the
			// batch one link at a time so valid links still get queued.
			for _, link := range batch {
				single, singleErr := client.Add(ctx, []string{link})
				if singleErr != nil {
					hadError = true
					failures = append(failures, map[string]string{"link": link, "error": singleErr.Error()})
					fmt.Fprintf(a.Stderr, "%s: %v\n", link, singleErr)
					if ctx.Err() != nil {
						return false
					}
					continue
				}
				all = append(all, single.Jobs...)
				created += single.Created
				existing += single.Existing
			}
			return true
		}
		fmt.Fprintf(a.Stderr, "magnet add: %v\n", err)
		hadError = true
		failures = append(failures, map[string]string{"error": err.Error()})
		return false
	}

	for start := 0; start < len(validLinks); start += *batchSize {
		end := start + *batchSize
		if end > len(validLinks) {
			end = len(validLinks)
		}
		if !addBatch(validLinks[start:end]) {
			if ctx.Err() != nil {
				return 130
			}
			break
		}
	}

	if *jsonOut {
		printJSON(a.Stdout, map[string]any{
			"jobs":     all,
			"created":  created,
			"existing": existing,
			"errors":   failures,
		})
		if hadError {
			return 1
		}
		return 0
	}
	if *quiet {
		for _, j := range all {
			fmt.Fprintln(a.Stdout, j.ID)
		}
		if hadError {
			return 1
		}
		return 0
	}
	if len(all) > 0 {
		tw := tabwriter.NewWriter(a.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tNAME")
		for _, j := range all {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", j.ID, j.State, displayName(j))
		}
		tw.Flush()
	}
	fmt.Fprintf(a.Stderr, "added: %d, existing: %d, errors: %d\n", created, existing, len(failures))
	if ctx.Err() != nil {
		return 130
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientList(args []string) int {
	fs := newFlagSet("ls", a.Stderr)
	server, token := clientConnFlags(fs)
	state := stringFlag(fs, "state", "", "", "state filter: queued, searching, found, incomplete, failed, canceled")
	limit := intFlag(fs, "limit", "n", 100, "page size")
	all := boolFlag(fs, "all", "a", false, "load all pages")
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "print only IDs")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	if *limit < 1 {
		fmt.Fprintln(a.Stderr, "magnet ls: --limit must be greater than zero")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)

	var jobs []model.Job
	total := 0
	offset := 0
	for {
		pageSize := *limit
		if *all && pageSize < 500 {
			pageSize = 500
		}
		res, err := client.List(ctx, model.State(*state), pageSize, offset)
		if err != nil {
			fmt.Fprintf(a.Stderr, "magnet ls: %v\n", err)
			return 1
		}
		total = res.Total
		jobs = append(jobs, res.Jobs...)
		if !*all || len(res.Jobs) == 0 || offset+len(res.Jobs) >= res.Total {
			break
		}
		offset += len(res.Jobs)
	}

	if *jsonOut {
		limitOut := *limit
		if *all {
			limitOut = len(jobs)
		}
		printJSON(a.Stdout, api.ListResult{Jobs: jobs, Total: total, Limit: limitOut, Offset: 0})
		return 0
	}
	if len(jobs) == 0 {
		if !*quiet {
			fmt.Fprintln(a.Stderr, "no jobs")
		}
		return 0
	}
	if *quiet {
		for _, j := range jobs {
			fmt.Fprintln(a.Stdout, j.ID)
		}
		return 0
	}
	printJobsTable(a.Stdout, a.Stderr, jobs, total)
	return 0
}

func (a *App) runClientShow(args []string) int {
	fs := newFlagSet("show", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet show: provide at least one job ID")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)

	var jobs []model.Job
	hadError := false
	for _, id := range ids {
		j, err := client.Get(ctx, id)
		if err != nil {
			fmt.Fprintf(a.Stderr, "magnet show %s: %v\n", id, err)
			hadError = true
			continue
		}
		jobs = append(jobs, j)
	}
	if *jsonOut {
		if len(ids) == 1 {
			if len(jobs) == 1 {
				printJSON(a.Stdout, jobs[0])
			}
		} else {
			printJSON(a.Stdout, jobs)
		}
	} else {
		for i, j := range jobs {
			if i > 0 {
				fmt.Fprintln(a.Stdout)
			}
			printJobDetail(a.Stdout, j)
		}
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientWait(args []string) int {
	fs := newFlagSet("wait", a.Stderr)
	server, token := clientConnFlags(fs)
	timeout := durationFlag(fs, "timeout", "t", 0, "overall wait timeout (0 = unlimited)")
	interval := durationFlag(fs, "interval", "", 2*time.Second, "daemon polling interval")
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print status messages")
	strict := boolFlag(fs, "strict", "", false, "treat incomplete as an error")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet wait: provide at least one job ID")
		return 2
	}
	if *interval <= 0 {
		fmt.Fprintln(a.Stderr, "magnet wait: --interval must be greater than zero")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	outcomes := waitForJobs(ctx, client, ids, *interval, *timeout)

	if *jsonOut {
		printJSON(a.Stdout, waitJSON(outcomes))
	} else {
		for _, o := range outcomes {
			if o.Err != nil {
				fmt.Fprintf(a.Stderr, "%s: %v\n", o.ID, o.Err)
				continue
			}
			switch o.Job.State {
			case model.StateFound:
				if o.Job.Path != "" {
					fmt.Fprintln(a.Stdout, o.Job.Path)
				}
				if !*quiet {
					fmt.Fprintf(a.Stderr, "%s: ready\n", o.ID)
				}
			case model.StateIncomplete:
				if o.Job.Path != "" {
					fmt.Fprintln(a.Stdout, o.Job.Path)
				}
				if o.Job.Warning != "" {
					fmt.Fprintf(a.Stderr, "%s: warning: %s\n", o.ID, o.Job.Warning)
				}
				if !*quiet {
					fmt.Fprintf(a.Stderr, "%s: ready (incomplete)\n", o.ID)
				}
			case model.StateFailed:
				fmt.Fprintf(a.Stderr, "%s: error: %s\n", o.ID, o.Job.Error)
			case model.StateCanceled:
				fmt.Fprintf(a.Stderr, "%s: canceled\n", o.ID)
			}
		}
	}
	if ctx.Err() != nil {
		return 130
	}
	for _, o := range outcomes {
		if o.Err != nil {
			return 1
		}
		if o.Job.State == model.StateIncomplete && !*strict {
			continue
		}
		if o.Job.State != model.StateFound {
			return 1
		}
	}
	return 0
}

func (a *App) runClientGet(args []string) int {
	fs := newFlagSet("get", a.Stderr)
	server, token := clientConnFlags(fs)
	out := stringFlag(fs, "out", "o", ".", "directory or .torrent file path")
	timeout := durationFlag(fs, "timeout", "t", 0, "overall wait timeout (0 = unlimited)")
	interval := durationFlag(fs, "interval", "", 2*time.Second, "daemon polling interval")
	force := boolFlag(fs, "force", "f", false, "overwrite an existing .torrent")
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print status messages")
	strict := boolFlag(fs, "strict", "", false, "treat incomplete as an error")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet get: provide at least one job ID")
		return 2
	}
	if *interval <= 0 {
		fmt.Fprintln(a.Stderr, "magnet get: --interval must be greater than zero")
		return 2
	}
	if len(ids) > 1 && strings.HasSuffix(strings.ToLower(*out), ".torrent") {
		fmt.Fprintln(a.Stderr, "magnet get: --out must be a directory when multiple jobs are given")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	outcomes := waitForJobs(ctx, client, ids, *interval, *timeout)

	type getResult struct {
		ID      string `json:"id"`
		Name    string `json:"name,omitempty"`
		Path    string `json:"path,omitempty"`
		State   string `json:"state"`
		Warning string `json:"warning,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]getResult, 0, len(outcomes))
	hadError := false
	for _, o := range outcomes {
		res := getResult{ID: o.ID, State: string(o.Job.State)}
		if o.Err != nil {
			res.Error = o.Err.Error()
			results = append(results, res)
			fmt.Fprintf(a.Stderr, "%s: %v\n", o.ID, o.Err)
			hadError = true
			continue
		}
		res.Name = o.Job.Name
		res.Warning = o.Job.Warning
		if o.Job.State != model.StateFound && o.Job.State != model.StateIncomplete {
			if o.Job.State == model.StateFailed {
				res.Error = o.Job.Error
			} else {
				res.Error = "job is not complete: " + string(o.Job.State)
			}
			results = append(results, res)
			fmt.Fprintf(a.Stderr, "%s: %s\n", o.ID, res.Error)
			hadError = true
			continue
		}
		if o.Job.State == model.StateIncomplete {
			if o.Job.Warning != "" {
				fmt.Fprintf(a.Stderr, "%s: warning: %s\n", o.ID, o.Job.Warning)
			}
			if *strict {
				hadError = true
			}
		}

		path, exists, err := fsutil.ResolveOutputPath(*out, o.Job.Name, o.Job.ID, *force)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			fmt.Fprintf(a.Stderr, "%s: %v\n", o.ID, err)
			hadError = true
			continue
		}
		if !exists {
			client.HTTP.Timeout = 5 * time.Minute
			body, _, err := client.OpenTorrent(ctx, o.Job.ID)
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				fmt.Fprintf(a.Stderr, "%s: download torrent: %v\n", o.ID, err)
				hadError = true
				continue
			}
			err = fsutil.AtomicWriteReader(path, body, 0o644)
			body.Close()
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				fmt.Fprintf(a.Stderr, "%s: save %s: %v\n", o.ID, path, err)
				hadError = true
				continue
			}
		}
		res.Path = path
		results = append(results, res)
		if !*jsonOut {
			fmt.Fprintln(a.Stdout, path)
		}
		if !*quiet {
			if exists {
				fmt.Fprintf(a.Stderr, "%s: file already exists: %s\n", o.ID, path)
			} else {
				fmt.Fprintf(a.Stderr, "%s: saved: %s\n", o.ID, path)
			}
		}
	}

	if *jsonOut {
		printJSON(a.Stdout, results)
	}
	if ctx.Err() != nil {
		return 130
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientCancel(args []string) int {
	fs := newFlagSet("cancel", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print status messages")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet cancel: provide at least one job ID")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	type result struct {
		ID    string     `json:"id"`
		Job   *model.Job `json:"job,omitempty"`
		Error string     `json:"error,omitempty"`
	}
	results := make([]result, 0, len(ids))
	hadError := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return 130
		}
		j, err := client.Cancel(ctx, id)
		r := result{ID: id}
		if err != nil {
			r.Error = err.Error()
			hadError = true
			fmt.Fprintf(a.Stderr, "magnet cancel %s: %v\n", id, err)
		} else {
			r.Job = &j
			if !*quiet {
				fmt.Fprintf(a.Stdout, "%s\t%s\n", j.ID, j.State)
			}
		}
		results = append(results, r)
	}
	if *jsonOut {
		printJSON(a.Stdout, results)
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientRetry(args []string) int {
	fs := newFlagSet("retry", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print status messages")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet retry: provide at least one job ID")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	type result struct {
		ID    string     `json:"id"`
		Job   *model.Job `json:"job,omitempty"`
		Error string     `json:"error,omitempty"`
	}
	results := make([]result, 0, len(ids))
	hadError := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return 130
		}
		j, err := client.Retry(ctx, id)
		r := result{ID: id}
		if err != nil {
			r.Error = err.Error()
			hadError = true
			fmt.Fprintf(a.Stderr, "magnet retry %s: %v\n", id, err)
		} else {
			r.Job = &j
			if !*quiet {
				fmt.Fprintf(a.Stdout, "%s\t%s\n", j.ID, j.State)
			}
		}
		results = append(results, r)
	}
	if *jsonOut {
		printJSON(a.Stdout, results)
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientRemove(args []string) int {
	fs := newFlagSet("rm", a.Stderr)
	server, token := clientConnFlags(fs)
	purge := boolFlag(fs, "purge", "", false, "also delete the saved .torrent")
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	quiet := boolFlag(fs, "quiet", "q", false, "do not print status messages")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(a.Stderr, "magnet rm: provide at least one job ID")
		return 2
	}

	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	type result struct {
		ID      string `json:"id"`
		Removed bool   `json:"removed"`
		Purged  bool   `json:"purged,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(ids))
	hadError := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return 130
		}
		err := client.Delete(ctx, id, *purge)
		r := result{ID: id, Purged: *purge}
		if err != nil {
			r.Error = err.Error()
			hadError = true
			fmt.Fprintf(a.Stderr, "magnet rm %s: %v\n", id, err)
		} else {
			r.Removed = true
			if !*quiet {
				fmt.Fprintf(a.Stdout, "%s\tremoved\n", id)
			}
		}
		results = append(results, r)
	}
	if *jsonOut {
		printJSON(a.Stdout, results)
	}
	if hadError {
		return 1
	}
	return 0
}

func (a *App) runClientStats(args []string) int {
	fs := newFlagSet("stats", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	resp, err := client.Stats(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet stats: %v\n", err)
		return 1
	}
	if *jsonOut {
		printJSON(a.Stdout, resp)
		return 0
	}
	s := resp.Stats
	fmt.Fprintf(a.Stdout, "total:       %d\n", s.Total)
	fmt.Fprintf(a.Stdout, "queued:      %d\n", s.Queued)
	fmt.Fprintf(a.Stdout, "searching:   %d\n", s.Searching)
	fmt.Fprintf(a.Stdout, "found:       %d\n", s.Found)
	fmt.Fprintf(a.Stdout, "incomplete:  %d\n", s.Incomplete)
	fmt.Fprintf(a.Stdout, "failed:      %d\n", s.Failed)
	fmt.Fprintf(a.Stdout, "canceled:    %d\n", s.Canceled)
	fmt.Fprintf(a.Stdout, "active:      %d\n", s.Active)
	fmt.Fprintf(a.Stdout, "uptime:     %s\n", shortDuration(time.Duration(resp.UptimeSeconds)*time.Second))
	return 0
}

func (a *App) runClientPing(args []string) int {
	fs := newFlagSet("ping", a.Stderr)
	server, token := clientConnFlags(fs)
	jsonOut := boolFlag(fs, "json", "", false, "output JSON")
	ok, code := parseFlags(fs, args)
	if !ok {
		return code
	}
	ctx, stop := a.signalContext()
	defer stop()
	client := api.NewClient(*server, *token)
	resp, err := client.Health(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "magnet ping: %v\n", err)
		return 1
	}
	if *jsonOut {
		printJSON(a.Stdout, resp)
		return 0
	}
	fmt.Fprintf(a.Stdout, "ok, version %s, uptime %s\n", resp.Version, shortDuration(time.Duration(resp.UptimeSeconds)*time.Second))
	return 0
}

func waitForJobs(ctx context.Context, client *api.Client, ids []string, interval, timeout time.Duration) []waitOutcome {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// Waiting must survive a daemon restart, but a dead endpoint should not make
	// the command hang silently forever.
	client.HTTP.Timeout = 15 * time.Second
	started := time.Now()
	reachable := false

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	pending := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		pending[id] = struct{}{}
	}
	outcomes := make(map[string]waitOutcome, len(ids))
	lastErr := make(map[string]error)

	for len(pending) > 0 {
		for id := range pending {
			if ctx.Err() != nil {
				break
			}
			j, err := client.Get(ctx, id)
			if err != nil {
				var apiErr *api.Error
				if errors.As(err, &apiErr) {
					reachable = true
					if apiErr.StatusCode == http.StatusNotFound {
						outcomes[id] = waitOutcome{ID: id, Err: err}
						delete(pending, id)
						continue
					}
				}
				lastErr[id] = err
				continue
			}
			reachable = true
			if j.Terminal() {
				outcomes[id] = waitOutcome{ID: id, Job: j}
				delete(pending, id)
			}
		}
		if len(pending) == 0 || ctx.Err() != nil {
			break
		}
		if !reachable && time.Since(started) > 5*time.Second {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	for id := range pending {
		err := lastErr[id]
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			if err != nil {
				err = fmt.Errorf("wait timeout after %s (last error: %v)", timeout, err)
			} else {
				err = fmt.Errorf("wait timeout after %s", timeout)
			}
		case ctx.Err() != nil:
			err = ctx.Err()
		case err == nil:
			err = errors.New("job did not complete")
		}
		outcomes[id] = waitOutcome{ID: id, Err: err}
	}

	ordered := make([]waitOutcome, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, outcomes[id])
	}
	return ordered
}

type waitOutcome struct {
	ID  string
	Job model.Job
	Err error
}

func waitJSON(outcomes []waitOutcome) []map[string]string {
	result := make([]map[string]string, 0, len(outcomes))
	for _, o := range outcomes {
		item := map[string]string{"id": o.ID}
		if o.Err != nil {
			item["error"] = o.Err.Error()
		} else {
			item["state"] = string(o.Job.State)
			item["path"] = o.Job.Path
			item["name"] = o.Job.Name
			item["version"] = o.Job.Version
			item["info_hash_v1"] = o.Job.InfoHashV1
			item["info_hash_v2"] = o.Job.InfoHashV2
			if o.Job.Warning != "" {
				item["warning"] = o.Job.Warning
			}
			if o.Job.Error != "" {
				item["error"] = o.Job.Error
			}
		}
		result = append(result, item)
	}
	return result
}

func printJobsTable(w, errW io.Writer, jobs []model.Job, total int) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tVER\tATT\tSIZE\tCREATED\tNAME")
	for _, j := range jobs {
		version := j.Version
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			j.ID, j.State, version, j.Attempts, formatBytes(j.Length), formatTime(j.CreatedAt), displayName(j))
	}
	tw.Flush()
	if total > len(jobs) {
		fmt.Fprintf(errW, "showing %d of %d (use --all or --limit)\n", len(jobs), total)
	}
}

func printJobDetail(w io.Writer, j model.Job) {
	fmt.Fprintf(w, "ID:          %s\n", j.ID)
	fmt.Fprintf(w, "State:       %s\n", j.State)
	if j.Version != "" {
		fmt.Fprintf(w, "Version:     %s\n", j.Version)
	}
	if j.InfoHashV1 != "" {
		fmt.Fprintf(w, "Info hash v1: %s\n", j.InfoHashV1)
	}
	if j.InfoHashV2 != "" {
		fmt.Fprintf(w, "Info hash v2: %s\n", j.InfoHashV2)
	}
	fmt.Fprintf(w, "Name:        %s\n", displayName(j))
	fmt.Fprintf(w, "Size:        %s\n", formatBytes(j.Length))
	fmt.Fprintf(w, "Files:       %d\n", j.Files)
	fmt.Fprintf(w, "Attempts:    %d\n", j.Attempts)
	fmt.Fprintf(w, "Created:     %s\n", formatTime(j.CreatedAt))
	fmt.Fprintf(w, "Updated:     %s\n", formatTime(j.UpdatedAt))
	if j.StartedAt != nil {
		fmt.Fprintf(w, "Started:     %s\n", formatTime(*j.StartedAt))
	}
	if j.FinishedAt != nil {
		fmt.Fprintf(w, "Finished:    %s\n", formatTime(*j.FinishedAt))
	}
	if j.NextAttemptAt != nil {
		fmt.Fprintf(w, "Next attempt: %s\n", formatTime(*j.NextAttemptAt))
	}
	if j.Path != "" {
		fmt.Fprintf(w, "File:        %s\n", j.Path)
	}
	if j.Warning != "" {
		fmt.Fprintf(w, "Warning:     %s\n", j.Warning)
	}
	if j.Error != "" {
		fmt.Fprintf(w, "Error:       %s\n", j.Error)
	}
	for i, magnet := range j.Magnets {
		if i == 0 && magnet == j.Magnet {
			continue
		}
		fmt.Fprintf(w, "Magnet:      %s\n", magnet)
	}
	if len(j.Aliases) > 0 {
		fmt.Fprintf(w, "Aliases:     %s\n", strings.Join(j.Aliases, ", "))
	}
	fmt.Fprintf(w, "Magnet:      %s\n", j.Magnet)
}

func displayName(j model.Job) string {
	if j.Name != "" {
		return j.Name
	}
	return "-"
}

func formatBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
