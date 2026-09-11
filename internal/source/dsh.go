package source

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liyixuan201211/agentlog/internal/model"
	"github.com/liyixuan201211/agentlog/internal/zstd"
)

// DSH reads DeepSeek Harness session stores.
//
// Layout: ~/.dsh/sessions/<slugified-cwd>/<session-id>/ holding a zstd
// compressed JSONL stream. Newer sessions use session.v3.jsonl.zstd and older
// ones session.jsonl.zstd; the formats overlap, so one parser handles both and
// the newer file wins when both are present.
type DSH struct {
	Root string
	// Backend selects the decompressor for the compressed streams.
	Backend zstd.Backend
}

// NewDSH returns an adapter for the default DSH store.
func NewDSH() *DSH {
	home := os.Getenv("HOME")
	return &DSH{Root: filepath.Join(home, ".dsh", "sessions"), Backend: zstd.BackendAuto}
}

func (d *DSH) Name() string { return "dsh" }

func (d *DSH) Discover() ([]Session, error) {
	slugs, err := os.ReadDir(d.Root)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		dirs, err := os.ReadDir(filepath.Join(d.Root, slug.Name()))
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			full := filepath.Join(d.Root, slug.Name(), dir.Name())
			// Prefer the newest format, then fall back to the legacy stream.
			candidates := []string{
				filepath.Join(full, "session.v3.jsonl.zstd"),
				filepath.Join(full, "session.jsonl.zstd"),
			}
			for _, path := range candidates {
				size, mod := fileInfo(path)
				if size == 0 {
					continue
				}
				out = append(out, Session{
					Source:  "dsh",
					ID:      dir.Name(),
					Path:    path,
					CWD:     dshCWD(slug.Name()),
					Bytes:   size,
					ModTime: mod,
				})
				break
			}
		}
	}
	return out, nil
}

// dshCWD approximates the working directory from a session directory slug.
func dshCWD(slug string) string {
	s := strings.Trim(slug, "-")
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "~", "/")
	return "/" + strings.ReplaceAll(s, "-", "/")
}

type dshRecord struct {
	Type string          `json:"type"`
	Seq  int             `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type dshContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Raw decompresses a session without parsing it.
func (d *DSH) Raw(s Session) ([]byte, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	return zstd.Decompress(raw, d.Backend)
}

// Load parses a DSH session into the neutral model.
func (d *DSH) Load(s Session) (*model.Transcript, error) {
	plain, err := d.Raw(s)
	if err != nil {
		return nil, err
	}

	tr := &model.Transcript{
		Source: "dsh",
		ID:     s.ID,
		Path:   s.Path,
		CWD:    s.CWD,
	}

	scanLinesFromBytes(plain, func(line []byte) error {
		var rec dshRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			tr.Malformed++
			return nil
		}
		if rec.Time > 0 {
			ts := time.UnixMilli(rec.Time)
			if tr.Started.IsZero() || ts.Before(tr.Started) {
				tr.Started = ts
			}
			if ts.After(tr.Ended) {
				tr.Ended = ts
			}
		}
		switch rec.Type {
		case "session":
			var meta struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(rec.Data, &meta) == nil && meta.CWD != "" {
				tr.CWD = meta.CWD
			}
		case "session/title":
			var t struct {
				Title string `json:"title"`
			}
			// A session can be retitled as it progresses, so the newest
			// title record wins.
			if json.Unmarshal(rec.Data, &t) == nil && t.Title != "" {
				tr.Title = t.Title
			}
		case "request/header":
			var h struct {
				Header struct {
					Config struct {
						Provider string `json:"provider"`
						Model    string `json:"model"`
					} `json:"config"`
				} `json:"header"`
			}
			if json.Unmarshal(rec.Data, &h) == nil {
				if m := h.Header.Config.Model; m != "" {
					tr.Model = m
				}
			}
		case "user/message":
			// Spliced inbox content also arrives as user messages, so both
			// shapes are handled.
			var m struct {
				Content []dshContent `json:"content"`
			}
			if json.Unmarshal(rec.Data, &m) != nil {
				var inbox struct {
					Inserted []struct {
						Content []dshContent `json:"content"`
					} `json:"inserted"`
				}
				if json.Unmarshal(rec.Data, &inbox) == nil {
					for _, ins := range inbox.Inserted {
						turn := turnFromContent(ins.Content, model.RoleUser)
						if len(turn.Blocks) > 0 {
							turn.Time = tr.Ended
							tr.Turns = append(tr.Turns, turn)
						}
					}
				}
				return nil
			}
			turn := turnFromContent(m.Content, model.RoleUser)
			if len(turn.Blocks) == 0 {
				return nil
			}
			turn.Time = tr.Ended
			if tr.Title == "" {
				if t := turn.Text(); strings.TrimSpace(t) != "" {
					tr.Title = t
				}
			}
			tr.Turns = append(tr.Turns, turn)
		case "agent/inbox/spliced":
			var inbox struct {
				Inserted []struct {
					Content []dshContent `json:"content"`
				} `json:"inserted"`
			}
			if json.Unmarshal(rec.Data, &inbox) != nil {
				return nil
			}
			for _, ins := range inbox.Inserted {
				turn := turnFromContent(ins.Content, model.RoleUser)
				if len(turn.Blocks) > 0 {
					turn.Time = tr.Ended
					tr.Turns = append(tr.Turns, turn)
				}
			}
		case "assistant/message":
			var m struct {
				Message struct {
					Role    string       `json:"role"`
					Content []dshContent `json:"content"`
				} `json:"message"`
				Usage *struct {
					InputTokens     int `json:"inputTokens"`
					OutputTokens    int `json:"outputTokens"`
					CacheReadTokens int `json:"cacheReadTokens"`
					ReasoningTokens int `json:"reasoningTokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(rec.Data, &m) != nil {
				return nil
			}
			if u := m.Usage; u != nil {
				tr.Usage.Add(model.Usage{
					Input:     u.InputTokens,
					Output:    u.OutputTokens,
					CacheRead: u.CacheReadTokens,
					Reasoning: u.ReasoningTokens,
				})
			}
			turn := model.Turn{Role: model.RoleAssistant, Time: tr.Ended}
			for _, c := range m.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						turn.Blocks = append(turn.Blocks, model.Block{Kind: "text", Text: c.Text})
					}
				case "reasoning":
					if strings.TrimSpace(c.Text) != "" {
						turn.Blocks = append(turn.Blocks, model.Block{Kind: "thinking", Text: c.Text})
					}
				}
			}
			if len(turn.Blocks) > 0 {
				tr.Turns = append(tr.Turns, turn)
			}
		case "tool/call":
			var c struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(rec.Data, &c) != nil {
				return nil
			}
			tr.Turns = append(tr.Turns, model.Turn{
				Role:      model.RoleAssistant,
				Time:      tr.Ended,
				Blocks:    []model.Block{{Kind: "tool_use", Name: c.Name, Input: c.Arguments}},
				ToolCalls: 1,
			})
		case "tool/result":
			var m struct {
				Message struct {
					Content []struct {
						Type    string `json:"type"`
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(rec.Data, &m) != nil {
				return nil
			}
			var sb strings.Builder
			for _, outer := range m.Message.Content {
				for _, inner := range outer.Content {
					if inner.Type == "text" {
						sb.WriteString(inner.Text)
					}
				}
			}
			if sb.Len() == 0 {
				return nil
			}
			tr.Turns = append(tr.Turns, model.Turn{
				Role:   model.RoleTool,
				Time:   tr.Ended,
				Blocks: []model.Block{{Kind: "tool_result", Text: sb.String()}},
			})
		case "assistant/attempt":
			tr.Usage.Add(dshAttemptUsage(rec.Data))
		}
		return nil
	})
	tr.DeriveTitle()
	return tr, nil
}

// dshAttemptUsage extracts token counts from an attempt record when present.
func dshAttemptUsage(data json.RawMessage) model.Usage {
	var attempt struct {
		Stream []struct {
			Chunk struct {
				Type  string `json:"type"`
				Usage *struct {
					InputTokens  int `json:"inputTokens"`
					OutputTokens int `json:"outputTokens"`
				} `json:"usage"`
			} `json:"chunk"`
		} `json:"stream"`
	}
	if json.Unmarshal(data, &attempt) != nil {
		return model.Usage{}
	}
	var out model.Usage
	for _, ev := range attempt.Stream {
		if ev.Chunk.Type == "usage" && ev.Chunk.Usage != nil {
			out.Input += ev.Chunk.Usage.InputTokens
			out.Output += ev.Chunk.Usage.OutputTokens
		}
	}
	return out
}

func turnFromContent(content []dshContent, role model.Role) model.Turn {
	turn := model.Turn{Role: role}
	for _, c := range content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			turn.Blocks = append(turn.Blocks, model.Block{Kind: "text", Text: c.Text})
		}
	}
	return turn
}

// scanLinesFromBytes is scanLines for an in-memory stream.
func scanLinesFromBytes(data []byte, fn func(line []byte) error) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i], data[i+1:]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return
		}
	}
}
