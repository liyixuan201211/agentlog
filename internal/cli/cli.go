// Package cli implements the agentlog command line interface.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liyixuan201211/agentlog/internal/model"
	"github.com/liyixuan201211/agentlog/internal/source"
)

// Run dispatches a subcommand and returns a process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "ls", "list":
		return cmdList(args[1:])
	case "search", "grep":
		return cmdSearch(args[1:])
	case "show", "cat":
		return cmdShow(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "stats":
		return cmdStats(args[1:])
	case "sources":
		return cmdSources(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "agentlog: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `agentlog reads the session history your AI coding agents leave on disk
and makes it searchable, readable and portable.

Usage:
  agentlog <command> [flags]

Commands:
  ls        List sessions across every detected agent harness
  search    Search message text across all sessions
  show      Print one session as a readable transcript
  export    Convert one session to Markdown or JSON
  stats     Token and session totals per harness and model
  sources   Show which harness stores were found

Run "agentlog <command> -h" for the flags of a command.

Examples:
  agentlog ls --limit 20
  agentlog search "rate limit" --source claude
  agentlog show --last --full
  agentlog export --last --format md > session.md
  agentlog stats --by model
`)
}

// common holds the flags shared by the session-selecting commands.
type common struct {
	sources string
	limit   int
	json    bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.sources, "source", "", "comma-separated harnesses to read (default: all detected)")
	fs.IntVar(&c.limit, "limit", 20, "maximum number of sessions to consider")
	fs.BoolVar(&c.json, "json", false, "emit JSON instead of text")
}

func (c *common) adapters() []source.Adapter {
	var names []string
	for _, n := range strings.Split(c.sources, ",") {
		if s := strings.TrimSpace(n); s != "" {
			names = append(names, s)
		}
	}
	return source.ByName(names)
}

func sessions(c *common) []source.Session {
	all := source.DiscoverAll(c.adapters())
	if c.limit > 0 && len(all) > c.limit {
		all = all[:c.limit]
	}
	return all
}

func cmdSources(args []string) int {
	fs := flag.NewFlagSet("sources", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	type info struct {
		Name    string `json:"name"`
		Found   bool   `json:"found"`
		Count   int    `json:"count"`
		Newest  string `json:"newest,omitempty"`
		Adapter string `json:"adapter"`
	}
	var out []info
	for _, a := range source.ByName(nil) {
		s, err := a.Discover()
		row := info{Name: a.Name(), Adapter: a.Name()}
		if err == nil {
			row.Found = true
			row.Count = len(s)
			if len(s) > 0 {
				newest := s[0].ModTime
				for _, x := range s {
					if x.ModTime.After(newest) {
						newest = x.ModTime
					}
				}
				row.Newest = model.FormatTime(newest)
			}
		}
		out = append(out, row)
	}
	if c.json {
		return printJSON(out)
	}
	fmt.Printf("%-8s %6s  %-16s %s\n", "SOURCE", "SESSIONS", "NEWEST", "STORE")
	for _, r := range out {
		status := "not found"
		if r.Found {
			status = ""
		}
		fmt.Printf("%-8s %6d  %-16s %s %s\n", r.Name, r.Count, r.Newest, r.Adapter, status)
	}
	return 0
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	var c common
	c.register(fs)
	noTitle := fs.Bool("no-title", false, "skip title lookup (faster on large stores)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	list := sessions(&c)
	if !*noTitle && !c.json {
		// Titles live inside the transcript, so the displayed rows are loaded
		// to fill them in. This stays cheap because list is bounded by --limit.
		adapterFor := map[string]source.Adapter{}
		for _, a := range c.adapters() {
			adapterFor[a.Name()] = a
		}
		for i := range list {
			if list[i].Title != "" {
				continue
			}
			a := adapterFor[list[i].Source]
			if a == nil {
				continue
			}
			if tr, err := a.Load(list[i]); err == nil {
				list[i].Title = tr.Title
			}
		}
	}
	if c.json {
		type row struct {
			Source  string `json:"source"`
			ID      string `json:"id"`
			Title   string `json:"title"`
			CWD     string `json:"cwd"`
			Bytes   int64  `json:"bytes"`
			ModTime string `json:"modified"`
			Path    string `json:"path"`
		}
		var rows []row
		for _, s := range list {
			rows = append(rows, row{
				Source: s.Source, ID: s.ID, Title: s.Title, CWD: s.CWD,
				Bytes: s.Bytes, ModTime: s.ModTime.Format(time.RFC3339), Path: s.Path,
			})
		}
		return printJSON(rows)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "agentlog: no sessions found")
		return 1
	}
	fmt.Printf("%-7s %-9s %-16s %8s  %s\n", "SOURCE", "ID", "MODIFIED", "SIZE", "TITLE")
	for _, s := range list {
		fmt.Printf("%-7s %-9s %-16s %8s  %s\n",
			s.Source, shortID(s.ID), model.FormatTime(s.ModTime),
			humanBytes(s.Bytes), truncate(s.Title, 60))
	}
	return 0
}

// resolve picks the session a command should act on from an id fragment, or
// the most recent one when no fragment is given.
func resolve(c *common, frag string, last bool) (source.Adapter, source.Session, error) {
	adapters := c.adapters()
	all := source.DiscoverAll(adapters)
	if len(all) == 0 {
		return nil, source.Session{}, fmt.Errorf("no sessions found")
	}
	var chosen source.Session
	switch {
	case frag == "":
		chosen = all[0]
	default:
		frag = strings.ToLower(frag)
		matches := make([]source.Session, 0, 2)
		for _, s := range all {
			if strings.Contains(strings.ToLower(s.ID), frag) || strings.Contains(strings.ToLower(s.Path), frag) {
				matches = append(matches, s)
			}
		}
		switch len(matches) {
		case 0:
			return nil, source.Session{}, fmt.Errorf("no session matches %q", frag)
		case 1:
			chosen = matches[0]
		default:
			// Prefer an exact id match, otherwise refuse to guess.
			exact := matches[:0]
			for _, m := range matches {
				if strings.EqualFold(m.ID, frag) {
					exact = append(exact, m)
				}
			}
			if len(exact) == 1 {
				chosen = exact[0]
				break
			}
			return nil, source.Session{}, fmt.Errorf("%q matches %d sessions; use a longer id", frag, len(matches))
		}
	}
	for _, a := range adapters {
		if a.Name() == chosen.Source {
			return a, chosen, nil
		}
	}
	return nil, source.Session{}, fmt.Errorf("adapter for %q is unavailable", chosen.Source)
}

func cmdShow(args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	var c common
	c.register(fs)
	full := fs.Bool("full", false, "include reasoning and tool output")
	noTools := fs.Bool("no-tools", false, "omit tool calls and results")
	last := fs.Bool("last", false, "use the most recent session")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	adapter, sess, err := resolve(&c, fs.Arg(0), *last)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}
	tr, err := adapter.Load(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}
	if c.json {
		return printJSON(tr)
	}
	printTranscript(os.Stdout, tr, *full, *noTools)
	return 0
}

func cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	var c common
	c.register(fs)
	format := fs.String("format", "md", "output format: md or json")
	full := fs.Bool("full", false, "include reasoning and tool output")
	out := fs.String("out", "", "write to a file instead of stdout")
	last := fs.Bool("last", false, "use the most recent session")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	adapter, sess, err := resolve(&c, fs.Arg(0), *last)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}
	tr, err := adapter.Load(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}

	var data []byte
	switch strings.ToLower(*format) {
	case "json":
		data, err = json.MarshalIndent(tr, "", "  ")
		if err == nil {
			data = append(data, '\n')
		}
	case "md", "markdown":
		var sb strings.Builder
		writeMarkdown(&sb, tr, *full)
		data = []byte(sb.String())
	default:
		fmt.Fprintf(os.Stderr, "agentlog: unknown format %q (want md or json)\n", *format)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}

	if *out == "" {
		os.Stdout.Write(data)
		return 0
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(data))
	return 0
}

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	var c common
	c.register(fs)
	by := fs.String("by", "source", "group by: source or model")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	adapters := c.adapters()
	all := source.DiscoverAll(adapters)
	if c.limit > 0 && len(all) > c.limit {
		all = all[:c.limit]
	}

	type bucket struct {
		Key      string      `json:"key"`
		Sessions int         `json:"sessions"`
		Messages int         `json:"messages"`
		ToolCall int         `json:"tool_calls"`
		Chars    int         `json:"chars"`
		Usage    model.Usage `json:"usage"`
		First    time.Time   `json:"-"`
		Last     time.Time   `json:"-"`
	}
	buckets := map[string]*bucket{}
	adapterFor := map[string]source.Adapter{}
	for _, a := range adapters {
		adapterFor[a.Name()] = a
	}

	failed := 0
	for _, s := range all {
		a := adapterFor[s.Source]
		if a == nil {
			continue
		}
		tr, err := a.Load(s)
		if err != nil {
			failed++
			continue
		}
		key := s.Source
		if *by == "model" {
			key = tr.Model
			if key == "" {
				key = "(unknown)"
			}
		}
		b := buckets[key]
		if b == nil {
			b = &bucket{Key: key}
			buckets[key] = b
		}
		b.Sessions++
		b.Messages += tr.Messages()
		b.ToolCall += tr.ToolCalls()
		for _, t := range tr.Turns {
			b.Chars += len(t.AllText())
		}
		b.Usage.Add(tr.Usage)
		if !tr.Started.IsZero() && (b.First.IsZero() || tr.Started.Before(b.First)) {
			b.First = tr.Started
		}
		if tr.Ended.After(b.Last) {
			b.Last = tr.Ended
		}
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return buckets[keys[i]].Usage.Total() > buckets[keys[j]].Usage.Total()
	})

	if c.json {
		var rows []*bucket
		for _, k := range keys {
			rows = append(rows, buckets[k])
		}
		return printJSON(rows)
	}

	fmt.Printf("%-28s %8s %8s %7s %10s %12s\n",
		strings.ToUpper(*by), "SESSIONS", "MSGS", "TOOLS", "TOKENS", "CHARS")
	var totalTok, totalSess, totalMsgs int
	for _, k := range keys {
		b := buckets[k]
		tok := b.Usage.Total()
		totalTok += tok
		totalSess += b.Sessions
		totalMsgs += b.Messages
		fmt.Printf("%-28s %8d %8d %7d %10s %12s\n",
			truncate(b.Key, 28), b.Sessions, b.Messages, b.ToolCall,
			model.FormatCount(tok), model.FormatCount(b.Chars))
	}
	fmt.Printf("%-28s %8d %8d %7s %10s\n", "TOTAL", totalSess, totalMsgs, "", model.FormatCount(totalTok))
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "agentlog: %d session(s) could not be read\n", failed)
	}
	return 0
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "agentlog: %v\n", err)
		return 1
	}
	return 0
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "session-")
	if len(id) > 9 {
		return id[:9]
	}
	return id
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGT"[exp])
}

// filepath is used by export's -out handling on some platforms; keep the
// import meaningful for path cleaning.
var _ = filepath.Clean
