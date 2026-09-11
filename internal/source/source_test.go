package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liyixuan201211/agentlog/internal/model"
)

// writeFixture creates a temporary session file and returns its path.
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeParsesTurnsAndUsage(t *testing.T) {
	// Two records: a user prompt and an assistant reply with reasoning, prose,
	// a tool call and token usage.
	const fixture = `{"type":"user","timestamp":"2026-01-01T10:00:00.000Z","cwd":"/work","message":{"role":"user","content":"hello there"}}
{"type":"assistant","timestamp":"2026-01-01T10:00:05.000Z","cwd":"/work","message":{"role":"assistant","model":"test-model","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"hi!"},{"type":"tool_use","name":"bash","input":{"command":"ls"}}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":7,"output_tokens_details":{"thinking_tokens":5}}}}
`
	path := writeFixture(t, "abc.jsonl", fixture)
	a := &Claude{Root: filepath.Dir(path)}
	tr, err := a.Load(Session{Source: "claude", ID: "abc", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if tr.CWD != "/work" {
		t.Errorf("CWD = %q, want /work", tr.CWD)
	}
	if tr.Model != "test-model" {
		t.Errorf("Model = %q", tr.Model)
	}
	if got := tr.Messages(); got != 2 {
		t.Errorf("Messages = %d, want 2", got)
	}
	if got := tr.ToolCalls(); got != 1 {
		t.Errorf("ToolCalls = %d, want 1", got)
	}
	if tr.Usage.Input != 100 || tr.Usage.Output != 20 || tr.Usage.CacheRead != 7 || tr.Usage.Reasoning != 5 {
		t.Errorf("Usage = %+v", tr.Usage)
	}
	if tr.Title != "hello there" {
		t.Errorf("Title = %q", tr.Title)
	}
	// Reasoning is captured but kept out of the prose text.
	if tr.Turns[1].Text() != "hi!" {
		t.Errorf("assistant text = %q", tr.Turns[1].Text())
	}
}

func TestCodexParsesRollout(t *testing.T) {
	const fixture = `{"timestamp":"2026-02-01T09:00:00.000Z","type":"session_meta","payload":{"id":"sess-1","cwd":"/repo"}}
{"timestamp":"2026-02-01T09:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}
{"timestamp":"2026-02-01T09:00:02.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"harness instructions"}]}}
{"timestamp":"2026-02-01T09:00:03.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
{"timestamp":"2026-02-01T09:00:04.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"ls -la"}}
{"timestamp":"2026-02-01T09:00:05.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"output_tokens":60,"cached_input_tokens":400,"reasoning_output_tokens":12}}}}
`
	path := writeFixture(t, "rollout-2026-02-01T09-00-00-abc.jsonl", fixture)
	a := &Codex{Root: filepath.Dir(path)}
	tr, err := a.Load(Session{Source: "codex", ID: "abc", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if tr.CWD != "/repo" {
		t.Errorf("CWD = %q, want /repo", tr.CWD)
	}
	if tr.ID != "sess-1" {
		t.Errorf("ID = %q, want sess-1 (from session_meta)", tr.ID)
	}
	// The developer record is harness instructions, not conversation, so the
	// two real messages plus the tool-call turn are all that remain.
	if got := tr.Messages(); got != 3 {
		t.Errorf("Messages = %d, want 3", got)
	}
	if got := tr.ToolCalls(); got != 1 {
		t.Errorf("ToolCalls = %d, want 1", got)
	}
	for _, turn := range tr.Turns {
		if turn.Role == model.RoleSystem {
			t.Errorf("developer record leaked in as a system turn")
		}
		for _, b := range turn.Blocks {
			if b.Text == "harness instructions" {
				t.Errorf("developer instructions were captured as conversation")
			}
		}
	}
	// Usage is cumulative in the source, so it is taken once, not summed.
	if tr.Usage.Input != 500 || tr.Usage.CacheRead != 400 || tr.Usage.Reasoning != 12 {
		t.Errorf("Usage = %+v", tr.Usage)
	}
}

func TestModelTitleDerivation(t *testing.T) {
	tr := &model.Transcript{
		Turns: []model.Turn{
			{Role: model.RoleUser, Blocks: []model.Block{{Kind: "text", Text: "  first line\nsecond line"}}},
		},
	}
	tr.DeriveTitle()
	if tr.Title != "first line" {
		t.Errorf("Title = %q, want %q", tr.Title, "first line")
	}
}
