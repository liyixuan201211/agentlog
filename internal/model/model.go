// Package model defines the harness-neutral transcript representation that
// every source adapter converts into.
//
// Nothing here depends on a particular agent CLI: a Transcript is what a
// session looks like once its native format has been understood, and a Turn is
// one message with its text, reasoning, and tool activity flattened into a
// form that can be printed, searched, or exported.
package model

import (
	"fmt"
	"strings"
	"time"
)

// Role identifies who produced a turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Block is one piece of a message: prose, reasoning, or a tool interaction.
type Block struct {
	// Kind is one of "text", "thinking", "tool_use" or "tool_result".
	Kind string
	Text string
	// Name is the tool name for tool blocks.
	Name string
	// Input is a short rendering of the tool arguments.
	Input string
	// IsError marks a failed tool result.
	IsError bool
}

// Turn is a single message in a transcript.
type Turn struct {
	Role      Role
	Time      time.Time
	Blocks    []Block
	Model     string
	ToolCalls int
}

// Text returns all prose blocks joined, ignoring reasoning and tool payloads.
func (t Turn) Text() string {
	var parts []string
	for _, b := range t.Blocks {
		if b.Kind == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// AllText returns every block's text, used for searching.
func (t Turn) AllText() string {
	var parts []string
	for _, b := range t.Blocks {
		switch b.Kind {
		case "text", "thinking":
			parts = append(parts, b.Text)
		case "tool_use":
			parts = append(parts, b.Name+" "+b.Input)
		case "tool_result":
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Usage records token counts for one assistant turn.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Reasoning  int
}

// Add accumulates another usage record.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
}

// Total is the sum of every token category.
func (u Usage) Total() int {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite + u.Reasoning
}

// Transcript is one session read from one agent harness.
type Transcript struct {
	// Source is the harness name, e.g. "claude" or "codex".
	Source string
	// ID is the session identifier as the harness recorded it.
	ID string
	// Path is the file the transcript was read from.
	Path string
	// CWD is the working directory the session ran in, when known.
	CWD string
	// Title is a short human label, usually the first user message.
	Title string
	// Started and Ended bound the session in time.
	Started time.Time
	Ended   time.Time
	// Model is the most frequently used model, for display.
	Model string
	// Turns holds the conversation in order.
	Turns []Turn
	// Usage aggregates token counts across assistant turns.
	Usage Usage
	// Malformed counts records that could not be parsed, so a reader can
	// report partial data honestly instead of silently dropping it.
	Malformed int
}

// Messages counts turns belonging to the conversation proper, ignoring
// synthetic system turns.
func (t *Transcript) Messages() int {
	n := 0
	for _, turn := range t.Turns {
		if turn.Role == RoleUser || turn.Role == RoleAssistant {
			n++
		}
	}
	return n
}

// ToolCalls counts tool invocations across the transcript.
func (t *Transcript) ToolCalls() int {
	n := 0
	for _, turn := range t.Turns {
		n += turn.ToolCalls
	}
	return n
}

// Duration is the wall-clock span of the session.
func (t *Transcript) Duration() time.Duration {
	if t.Started.IsZero() || t.Ended.IsZero() {
		return 0
	}
	return t.Ended.Sub(t.Started)
}

// ShortID trims a long identifier for table display.
func (t *Transcript) ShortID() string {
	id := t.ID
	id = strings.TrimPrefix(id, "session-")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DeriveTitle fills in a title from the first user turn when the harness did
// not provide one, and normalises whitespace for display.
func (t *Transcript) DeriveTitle() {
	if strings.TrimSpace(t.Title) == "" {
		for _, turn := range t.Turns {
			if turn.Role != RoleUser {
				continue
			}
			if s := firstLine(turn.Text()); s != "" {
				t.Title = s
				break
			}
		}
	}
	t.Title = oneLine(t.Title)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 100
	if len(s) > max {
		r := []rune(s)
		if len(r) > max {
			return string(r[:max-1]) + "…"
		}
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// FormatTime renders a timestamp consistently across the CLI.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// FormatCount renders a token count compactly.
func FormatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
