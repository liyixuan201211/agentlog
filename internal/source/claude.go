package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liyixuan201211/agentlog/internal/model"
)

// Claude reads Claude Code transcripts.
//
// Layout: ~/.claude/projects/<slugified-cwd>/<session-uuid>.jsonl, one JSON
// object per line, with a sibling directory holding sub-agent sidechains.
type Claude struct {
	Root string
}

// NewClaude returns an adapter for the default Claude Code store.
func NewClaude() *Claude {
	home := os.Getenv("HOME")
	return &Claude{Root: filepath.Join(home, ".claude", "projects")}
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) Discover() ([]Session, error) {
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(c.Root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			size, mod := fileInfo(path)
			out = append(out, Session{
				Source:  "claude",
				ID:      strings.TrimSuffix(f.Name(), ".jsonl"),
				Path:    path,
				CWD:     claudeCWD(e.Name()),
				Bytes:   size,
				ModTime: mod,
			})
		}
	}
	return out, nil
}

// claudeCWD turns a project directory slug back into a readable path. The
// encoding replaces path separators with dashes, which is lossy, so this only
// approximates the original directory.
func claudeCWD(slug string) string {
	if !strings.HasPrefix(slug, "-") {
		return ""
	}
	return strings.ReplaceAll(slug, "-", "/")
}

type claudeRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	CWD       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokensDetails      *struct {
			ThinkingTokens int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// Load parses a Claude Code transcript into the neutral model.
func (c *Claude) Load(s Session) (*model.Transcript, error) {
	tr := &model.Transcript{
		Source: "claude",
		ID:     s.ID,
		Path:   s.Path,
		CWD:    s.CWD,
	}
	models := map[string]int{}

	err := scanLines(s.Path, func(line []byte) error {
		var rec claudeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			tr.Malformed++
			return nil
		}
		if ts, ok := parseTime(rec.Timestamp); ok {
			if tr.Started.IsZero() || ts.Before(tr.Started) {
				tr.Started = ts
			}
			if ts.After(tr.Ended) {
				tr.Ended = ts
			}
		}
		if rec.CWD != "" {
			tr.CWD = rec.CWD
		}
		switch rec.Type {
		case "user":
			turn, ok := claudeTurn(rec.Message, model.RoleUser)
			if !ok {
				return nil
			}
			turn.Time = tr.Ended
			if tr.Title == "" {
				if t := turn.Text(); strings.TrimSpace(t) != "" {
					tr.Title = t
				}
			}
			tr.Turns = append(tr.Turns, turn)
		case "assistant":
			turn, ok := claudeTurn(rec.Message, model.RoleAssistant)
			if !ok {
				return nil
			}
			turn.Time = tr.Ended
			var msg claudeMessage
			if err := json.Unmarshal(rec.Message, &msg); err == nil {
				turn.Model = msg.Model
				if msg.Model != "" {
					models[msg.Model]++
				}
				if u := msg.Usage; u != nil {
					usage := model.Usage{
						Input:      u.InputTokens,
						Output:     u.OutputTokens,
						CacheRead:  u.CacheReadInputTokens,
						CacheWrite: u.CacheCreationInputTokens,
					}
					if u.OutputTokensDetails != nil {
						usage.Reasoning = u.OutputTokensDetails.ThinkingTokens
					}
					tr.Usage.Add(usage)
				}
			}
			for _, b := range turn.Blocks {
				if b.Kind == "tool_use" {
					turn.ToolCalls++
				}
			}
			tr.Turns = append(tr.Turns, turn)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	tr.Model = mostCommon(models)
	tr.DeriveTitle()
	return tr, nil
}

// claudeTurn converts a message payload into a Turn. It reports false when the
// payload carries neither prose nor tool activity, which is common for records
// that only exist to hold a snapshot.
func claudeTurn(raw json.RawMessage, role model.Role) (model.Turn, bool) {
	turn := model.Turn{Role: role}
	var msg claudeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return turn, false
	}

	// Content is either a plain string or an array of typed blocks.
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		if strings.TrimSpace(text) != "" {
			turn.Blocks = append(turn.Blocks, model.Block{Kind: "text", Text: text})
		}
		return turn, len(turn.Blocks) > 0
	}

	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
		IsError  bool            `json:"is_error"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return turn, false
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				turn.Blocks = append(turn.Blocks, model.Block{Kind: "text", Text: b.Text})
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				turn.Blocks = append(turn.Blocks, model.Block{Kind: "thinking", Text: b.Thinking})
			}
		case "tool_use":
			turn.Blocks = append(turn.Blocks, model.Block{
				Kind:  "tool_use",
				Name:  b.Name,
				Input: summariseJSON(b.Input),
			})
		case "tool_result":
			turn.Blocks = append(turn.Blocks, model.Block{
				Kind:    "tool_result",
				Text:    summariseJSON(b.Content),
				IsError: b.IsError,
			})
		}
	}
	return turn, len(turn.Blocks) > 0
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func mostCommon(m map[string]int) string {
	best, bestN := "", 0
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}
