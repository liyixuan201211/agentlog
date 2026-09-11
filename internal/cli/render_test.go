package cli

import (
	"strings"
	"testing"

	"github.com/liyixuan201211/agentlog/internal/model"
)

func TestSnippetCentresOnMatch(t *testing.T) {
	text := "the quick brown fox jumps over the lazy dog"
	idx := strings.Index(text, "fox")
	got := snippet(text, idx, len("fox"), 5)
	if !strings.Contains(got, "fox") {
		t.Errorf("snippet = %q, does not contain the match", got)
	}
	// The window includes the characters immediately before the match, and
	// marks that text was cut off on both sides.
	if !strings.Contains(got, "row") {
		t.Errorf("snippet = %q, does not include preceding context", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("snippet = %q, should mark truncation on both sides", got)
	}
}

func TestHoistFlagsAcceptsBothOrders(t *testing.T) {
	// Query before flags.
	got := hoistFlags([]string{"hello", "--limit", "5"})
	if got[0] != "--limit" || got[1] != "5" || got[2] != "hello" {
		t.Errorf("hoistFlags = %v", got)
	}
	// Flags before query.
	got = hoistFlags([]string{"--limit", "5", "hello"})
	if got[0] != "--limit" || got[2] != "hello" {
		t.Errorf("hoistFlags = %v", got)
	}
	// A multi-word query keeps its word order.
	got = hoistFlags([]string{"rate", "limit", "--json"})
	if strings.Join(got[1:], " ") != "rate limit" {
		t.Errorf("query words reordered: %v", got)
	}
}

func TestMarkdownExportContainsFrontMatter(t *testing.T) {
	tr := &model.Transcript{
		Source: "dsh",
		ID:     "sess-1",
		CWD:    "/work",
		Model:  "m1",
		Turns: []model.Turn{
			{Role: model.RoleUser, Blocks: []model.Block{{Kind: "text", Text: "hi"}}},
			{Role: model.RoleAssistant, Blocks: []model.Block{{Kind: "text", Text: "hello"}}},
		},
	}
	tr.DeriveTitle()
	var sb strings.Builder
	writeMarkdown(&sb, tr, false)
	out := sb.String()
	for _, want := range []string{"# hi", "| Harness | `dsh` |", "## User", "## Assistant", "hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output missing %q\n%s", want, out)
		}
	}
}
