package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/liyixuan201211/agentlog/internal/model"
	"github.com/liyixuan201211/agentlog/internal/source"
)

// hit is one matching turn.
type hit struct {
	Source  string `json:"source"`
	Session string `json:"session"`
	Title   string `json:"title"`
	Role    string `json:"role"`
	Time    string `json:"time"`
	Line    int    `json:"turn"`
	Snippet string `json:"snippet"`
}

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var c common
	c.register(fs)
	c.limit = 0
	context := fs.Int("context", 0, "characters of context around each match")
	turnOnly := fs.Bool("turns", false, "search conversation turns only, ignoring tool output")
	// Go's flag package stops at the first positional argument, so flags are
	// hoisted ahead of the query to accept both "search -x q" and "search q -x".
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "agentlog: search needs a query")
		return 2
	}
	needle := strings.ToLower(strings.Join(fs.Args(), " "))

	adapters := c.adapters()
	all := source.DiscoverAll(adapters)
	adapterFor := map[string]source.Adapter{}
	for _, a := range adapters {
		adapterFor[a.Name()] = a
	}

	// Comparing against the raw (decompressed) bytes first avoids paying for a
	// full parse in the common case where a session cannot match at all.
	needleBytes := []byte(needle)

	var hits []hit
	scanned, unreadable, skipped := 0, 0, 0
	for _, s := range all {
		a := adapterFor[s.Source]
		if a == nil {
			continue
		}
		if raw, ok := source.RawText(a, s); ok {
			if !bytes.Contains(bytes.ToLower(raw), needleBytes) {
				skipped++
				continue
			}
		}
		tr, err := a.Load(s)
		if err != nil {
			unreadable++
			continue
		}
		scanned++
		for i, turn := range tr.Turns {
			if *turnOnly && turn.Role != model.RoleUser && turn.Role != model.RoleAssistant {
				continue
			}
			text := turn.AllText()
			idx := strings.Index(strings.ToLower(text), needle)
			if idx < 0 {
				continue
			}
			hits = append(hits, hit{
				Source:  tr.Source,
				Session: tr.ShortID(),
				Title:   tr.Title,
				Role:    string(turn.Role),
				Time:    model.FormatTime(turn.Time),
				Line:    i,
				Snippet: snippet(text, idx, len(needle), *context),
			})
			if c.limit > 0 && len(hits) >= c.limit {
				break
			}
		}
		if c.limit > 0 && len(hits) >= c.limit {
			break
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Time > hits[j].Time })
	if c.json {
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Fprintf(os.Stderr, "agentlog: no matches in %d session(s)\n", scanned)
		return 1
	}
	for _, h := range hits {
		fmt.Printf("%s  %-6s %-9s %-9s %s\n", h.Time, h.Source, h.Session, h.Role, truncate(h.Title, 40))
		fmt.Printf("    %s\n", h.Snippet)
	}
	fmt.Fprintf(os.Stderr, "\n%d match(es) in %d matching session(s), %d pre-filtered",
		len(hits), scanned, skipped)
	if unreadable > 0 {
		fmt.Fprintf(os.Stderr, ", %d unreadable", unreadable)
	}
	fmt.Fprintln(os.Stderr)
	return 0
}

// snippet returns a single-line excerpt centred on a match.
func snippet(text string, idx, matchLen, context int) string {
	if context <= 0 {
		// Default to a readable window around the match.
		context = 80
	}
	start := idx - context
	if start < 0 {
		start = 0
	}
	end := idx + matchLen + context
	if end > len(text) {
		end = len(text)
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s = s + "…"
	}
	return s
}

// printTranscript renders a session for the terminal.
func printTranscript(w io.Writer, tr *model.Transcript, full, noTools bool) {
	fmt.Fprintf(w, "%s  %s\n", tr.Source, tr.ID)
	if tr.Title != "" {
		fmt.Fprintf(w, "title  %s\n", tr.Title)
	}
	if tr.CWD != "" {
		fmt.Fprintf(w, "cwd    %s\n", tr.CWD)
	}
	fmt.Fprintf(w, "when   %s → %s", model.FormatTime(tr.Started), model.FormatTime(tr.Ended))
	if d := tr.Duration(); d > 0 {
		fmt.Fprintf(w, "  (%s)", d.Round(time.Second))
	}
	fmt.Fprintln(w)
	if tr.Model != "" {
		fmt.Fprintf(w, "model  %s\n", tr.Model)
	}
	u := tr.Usage
	if u.Total() > 0 {
		fmt.Fprintf(w, "tokens in=%s out=%s cache_read=%s total=%s\n",
			model.FormatCount(u.Input), model.FormatCount(u.Output),
			model.FormatCount(u.CacheRead), model.FormatCount(u.Total()))
	}
	fmt.Fprintf(w, "%d messages, %d tool calls\n", tr.Messages(), tr.ToolCalls())
	fmt.Fprintln(w, strings.Repeat("─", 72))

	for _, turn := range tr.Turns {
		if noTools && turn.Role == model.RoleTool {
			continue
		}
		label := strings.ToUpper(string(turn.Role))
		if turn.Role == model.RoleAssistant && turn.Model != "" {
			label += " · " + turn.Model
		}
		fmt.Fprintf(w, "\n%s %s\n", label, strings.Repeat("·", max(0, 8-len(label))))
		for _, b := range turn.Blocks {
			switch b.Kind {
			case "text":
				fmt.Fprintln(w, strings.TrimRight(b.Text, "\n"))
			case "thinking":
				if !full {
					continue
				}
				fmt.Fprintf(w, "[thinking] %s\n", indentBlock(b.Text))
			case "tool_use":
				if noTools {
					continue
				}
				if full {
					fmt.Fprintf(w, "→ %s %s\n", b.Name, b.Input)
				} else {
					fmt.Fprintf(w, "→ %s\n", b.Name)
				}
			case "tool_result":
				if noTools || !full {
					continue
				}
				mark := "←"
				if b.IsError {
					mark = "✗"
				}
				fmt.Fprintf(w, "%s %s\n", mark, indentBlock(truncateMultiline(b.Text, 2000)))
			}
		}
	}
	if tr.Malformed > 0 {
		fmt.Fprintf(w, "\nnote: %d unparsable record(s) were skipped\n", tr.Malformed)
	}
}

func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.TrimLeft(strings.Join(lines, "\n"), " ")
}

func truncateMultiline(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n… (truncated)"
}

// writeMarkdown renders a session as a portable Markdown document.
func writeMarkdown(sb *strings.Builder, tr *model.Transcript, full bool) {
	fmt.Fprintf(sb, "# %s\n\n", titleOr(tr.Title, "Agent session "+tr.ShortID()))
	fmt.Fprintf(sb, "| | |\n|---|---|\n")
	fmt.Fprintf(sb, "| Harness | `%s` |\n", tr.Source)
	fmt.Fprintf(sb, "| Session | `%s` |\n", tr.ID)
	if tr.CWD != "" {
		fmt.Fprintf(sb, "| Directory | `%s` |\n", tr.CWD)
	}
	if tr.Model != "" {
		fmt.Fprintf(sb, "| Model | `%s` |\n", tr.Model)
	}
	fmt.Fprintf(sb, "| Started | %s |\n", model.FormatTime(tr.Started))
	fmt.Fprintf(sb, "| Ended | %s |\n", model.FormatTime(tr.Ended))
	fmt.Fprintf(sb, "| Messages | %d |\n", tr.Messages())
	fmt.Fprintf(sb, "| Tool calls | %d |\n", tr.ToolCalls())
	if u := tr.Usage; u.Total() > 0 {
		fmt.Fprintf(sb, "| Tokens | in %d / out %d / cache-read %d |\n", u.Input, u.Output, u.CacheRead)
	}
	fmt.Fprintln(sb)

	for _, turn := range tr.Turns {
		switch turn.Role {
		case model.RoleUser:
			sb.WriteString("## User\n\n")
		case model.RoleAssistant:
			sb.WriteString("## Assistant\n\n")
		case model.RoleTool:
			sb.WriteString("### Tool result\n\n")
		default:
			fmt.Fprintf(sb, "## %s\n\n", turn.Role)
		}
		for _, b := range turn.Blocks {
			switch b.Kind {
			case "text":
				fmt.Fprintf(sb, "%s\n\n", strings.TrimRight(b.Text, "\n"))
			case "thinking":
				if !full {
					continue
				}
				fmt.Fprintf(sb, "<details><summary>reasoning</summary>\n\n%s\n\n</details>\n\n",
					strings.TrimRight(b.Text, "\n"))
			case "tool_use":
				fmt.Fprintf(sb, "**Tool call: `%s`**\n\n", b.Name)
				if b.Input != "" {
					fmt.Fprintf(sb, "```json\n%s\n```\n\n", b.Input)
				}
			case "tool_result":
				if !full {
					continue
				}
				fmt.Fprintf(sb, "<details><summary>tool result%s</summary>\n\n```\n%s\n```\n\n</details>\n\n",
					errSuffix(b.IsError), strings.TrimRight(b.Text, "\n"))
			}
		}
	}
	if tr.Malformed > 0 {
		fmt.Fprintf(sb, "> %d record(s) in this session could not be parsed.\n", tr.Malformed)
	}
}

func errSuffix(isErr bool) string {
	if isErr {
		return " (error)"
	}
	return ""
}

func titleOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = os.Stdout

// hoistFlags moves flag-looking arguments ahead of positional ones so a query
// may be written before or after the flags. The literal "--" ends the scan.
func hoistFlags(args []string) []string {
	var flags, positional []string
	afterSep := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if afterSep {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			afterSep = true
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// A flag written as "--name value" consumes the next argument
			// unless it already carries "=".
			if !strings.Contains(a, "=") && flagNeedsValue(a) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// flagNeedsValue reports whether a boolean-looking flag is actually a switch.
func flagNeedsValue(name string) bool {
	switch strings.TrimLeft(name, "-") {
	case "json", "turns", "full", "no-tools", "last":
		return false
	}
	return true
}
