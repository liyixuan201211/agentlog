// Package source discovers and parses the session stores left behind by AI
// coding agent CLIs.
//
// Each harness keeps its own layout and its own record shapes, so every one
// gets an adapter that converts it into model.Transcript. Adapters are
// independent: adding a harness means adding a file, not touching the others.
package source

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liyixuan201211/agentlog/internal/model"
)

// Session is a transcript plus the metadata needed to list it without reading
// the whole file.
type Session struct {
	Source  string
	ID      string
	Path    string
	CWD     string
	Title   string
	Started time.Time
	Ended   time.Time
	Bytes   int64
	// ModTime is the file's modification time, used for cheap sorting.
	ModTime time.Time
}

// Adapter discovers and parses one harness's sessions.
type Adapter interface {
	// Name is the short source identifier used on the command line.
	Name() string
	// Discover lists sessions, newest first.
	Discover() ([]Session, error)
	// Load parses one session in full.
	Load(s Session) (*model.Transcript, error)
}

// All returns every adapter whose store exists on this machine.
func All() []Adapter {
	var out []Adapter
	for _, a := range []Adapter{NewClaude(), NewCodex(), NewDSH()} {
		if s, err := a.Discover(); err == nil && len(s) > 0 {
			out = append(out, a)
		}
	}
	return out
}

// ByName returns the adapters matching the given names, or all of them when
// names is empty.
func ByName(names []string) []Adapter {
	all := All()
	if len(names) == 0 {
		return all
	}
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []Adapter
	for _, a := range all {
		if want[a.Name()] {
			out = append(out, a)
		}
	}
	return out
}

// DiscoverAll lists sessions from every adapter, newest first.
func DiscoverAll(adapters []Adapter) []Session {
	var out []Session
	for _, a := range adapters {
		s, err := a.Discover()
		if err != nil {
			continue
		}
		out = append(out, s...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

// scanLines calls fn for each JSON line in a file, tolerating malformed or
// partially written trailing lines.
func scanLines(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Session lines embed whole file contents, so allow large records.
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// unquote turns a JSON string value into its decoded form, leaving the input
// untouched when it is not a JSON string.
func unquote(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// expandHome resolves a leading ~ against the user's home directory.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		if home = os.Getenv("HOME"); home == "" {
			return p
		}
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// fileInfo returns size and modification time, or zeroes on error.
func fileInfo(path string) (int64, time.Time) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}
	}
	return fi.Size(), fi.ModTime()
}

// summariseJSON renders a JSON value as a single compact line, truncating long
// payloads so a listing stays readable.
func summariseJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return oneLineText(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return oneLineText(string(raw))
	}
	return oneLineText(string(b))
}

// oneLineText collapses whitespace and caps the length for display.
func oneLineText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 200
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// RawText returns a session's unparsed bytes for a cheap substring pre-check.
//
// Compressed stores are deliberately excluded: decompressing a session just to
// reject it costs as much as parsing it, so callers skip the pre-check for
// those and let the adapter decide.
func RawText(a Adapter, s Session) ([]byte, bool) {
	if _, compressed := a.(RawReader); compressed {
		return nil, false
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// RawReader is implemented by adapters whose sessions are compressed on disk.
type RawReader interface {
	Raw(s Session) ([]byte, error)
}
