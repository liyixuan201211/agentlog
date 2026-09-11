package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/liyixuan201211/agentlog/internal/model"
)

// Codex reads Codex CLI and desktop rollout transcripts.
//
// Layout: ~/.codex/sessions/<yyyy>/<mm>/<dd>/rollout-*.jsonl, one JSON object
// per line carrying a payload whose "type" field names the record. Sessions
// open with a session_meta record holding the working directory.
type Codex struct {
	Root string
}

// NewCodex returns an adapter for the default Codex store.
func NewCodex() *Codex {
	home := os.Getenv("HOME")
	return &Codex{Root: filepath.Join(home, ".codex", "sessions")}
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Discover() ([]Session, error) {
	var out []Session
	err := filepath.Walk(c.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") || !strings.HasPrefix(filepath.Base(path), "rollout-") {
			return nil
		}
		out = append(out, Session{
			Source:  "codex",
			ID:      codexID(filepath.Base(path)),
			Path:    path,
			Bytes:   info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	return out, err
}

// codexID extracts the trailing UUID from a rollout file name.
func codexID(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	name = strings.TrimPrefix(name, "rollout-")
	if i := strings.LastIndex(name, "-"); i > 0 {
		// Timestamp and UUID are dash-joined; keep everything after the
		// timestamp for a stable, readable id.
		parts := strings.Split(name, "-")
		if len(parts) > 5 {
			return strings.Join(parts[5:], "-")
		}
	}
	return name
}

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type    string `json:"type"`
	CWD     string `json:"cwd"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Name    string `json:"name"`
	Input   string `json:"input"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Info *struct {
		TotalTokenUsage struct {
			InputTokens           int `json:"input_tokens"`
			CachedInputTokens     int `json:"cached_input_tokens"`
			CacheWriteInputTokens int `json:"cache_write_input_tokens"`
			OutputTokens          int `json:"output_tokens"`
			ReasoningOutputTokens int `json:"reasoning_output_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
}

// Load parses a Codex rollout transcript into the neutral model.
func (c *Codex) Load(s Session) (*model.Transcript, error) {
	tr := &model.Transcript{
		Source: "codex",
		ID:     s.ID,
		Path:   s.Path,
	}
	// Codex reports cumulative usage, so only the final record is counted.
	var lastUsage model.Usage
	seenUsage := false

	err := scanLines(s.Path, func(line []byte) error {
		var rec codexRecord
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
		var p codexPayload
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return nil
		}

		switch rec.Type {
		case "session_meta":
			if p.CWD != "" {
				tr.CWD = p.CWD
			}
			if p.ID != "" {
				tr.ID = p.ID
			}
		case "response_item":
			switch p.Type {
			case "message":
				role := model.Role(p.Role)
				switch role {
				case model.RoleUser, model.RoleAssistant, model.RoleSystem:
				default:
					// "developer" records carry harness instructions, not
					// conversation.
					return nil
				}
				var turn model.Turn
				turn.Role = role
				turn.Time = tr.Ended
				for _, b := range p.Content {
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					switch b.Type {
					case "input_text", "output_text", "text":
						turn.Blocks = append(turn.Blocks, model.Block{Kind: "text", Text: b.Text})
					}
				}
				if len(turn.Blocks) == 0 {
					return nil
				}
				if role == model.RoleUser && tr.Title == "" {
					tr.Title = turn.Text()
				}
				tr.Turns = append(tr.Turns, turn)
			case "reasoning":
				// Reasoning bodies are usually encrypted; only plaintext
				// summaries are useful.
				var summaries []string
				var raw struct {
					Summary []struct {
						Text string `json:"text"`
					} `json:"summary"`
				}
				if err := json.Unmarshal(rec.Payload, &raw); err == nil {
					for _, s := range raw.Summary {
						if strings.TrimSpace(s.Text) != "" {
							summaries = append(summaries, s.Text)
						}
					}
				}
				if len(summaries) == 0 {
					return nil
				}
				tr.Turns = append(tr.Turns, model.Turn{
					Role:   model.RoleAssistant,
					Time:   tr.Ended,
					Blocks: []model.Block{{Kind: "thinking", Text: strings.Join(summaries, "\n")}},
				})
			case "function_call", "custom_tool_call":
				tr.Turns = append(tr.Turns, model.Turn{
					Role: model.RoleAssistant,
					Time: tr.Ended,
					Blocks: []model.Block{{
						Kind:  "tool_use",
						Name:  p.Name,
						Input: p.Input,
					}},
					ToolCalls: 1,
				})
			case "function_call_output", "custom_tool_call_output":
				tr.Turns = append(tr.Turns, model.Turn{
					Role:   model.RoleTool,
					Time:   tr.Ended,
					Blocks: []model.Block{{Kind: "tool_result", Text: p.Input}},
				})
			}
		case "event_msg":
			if p.Type == "token_count" && p.Info != nil {
				u := p.Info.TotalTokenUsage
				lastUsage = model.Usage{
					Input:      u.InputTokens,
					Output:     u.OutputTokens,
					CacheRead:  u.CachedInputTokens,
					CacheWrite: u.CacheWriteInputTokens,
					Reasoning:  u.ReasoningOutputTokens,
				}
				seenUsage = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if seenUsage {
		tr.Usage = lastUsage
	}
	tr.DeriveTitle()
	return tr, nil
}
