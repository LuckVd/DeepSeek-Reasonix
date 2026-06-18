package snapshot

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"reasonix/internal/provider"
)

// mockProvider streams a fixed text chunk (or returns err from Stream).
type mockProvider struct {
	name  string
	out   string
	err   error
	calls int
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan provider.Chunk, 1)
	ch <- provider.Chunk{Type: provider.ChunkText, Text: m.out}
	close(ch)
	return ch, nil
}

var errBoom = errors.New("boom")

func TestParseSnapshot(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErr     bool
		wantPurpose string
	}{
		{"plain json", `{"purpose":"fix bug","progress":"x","deadEnds":[]}`, false, "fix bug"},
		{"fenced json", "```json\n{\"purpose\":\"y\"}\n```", false, "y"},
		{"json in prose", "prose then {\"purpose\":\"z\"} more", false, "z"},
		{"no json", "no json here at all", true, ""},
		{"empty", "", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := parseSnapshot(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("want error, got %+v", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.Purpose != c.wantPurpose {
				t.Errorf("purpose = %q, want %q", s.Purpose, c.wantPurpose)
			}
		})
	}
}

func TestGenerateParsesLLMOutput(t *testing.T) {
	mp := &mockProvider{out: `{"purpose":"修复登录","progress":"改auth","actions":[{"kind":"tool","summary":"edit auth.go"}],"deadEnds":["客户端刷新→跨域"],"nextStep":"后端静默刷新"}`}
	snap, err := Generate(context.Background(), Options{
		Prov:     mp,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "fix login"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap.Purpose != "修复登录" {
		t.Errorf("purpose = %q", snap.Purpose)
	}
	if len(snap.DeadEnds) != 1 {
		t.Errorf("deadEnds = %v, want 1", snap.DeadEnds)
	}
	if len(snap.Actions) != 1 {
		t.Fatalf("actions = %v, want 1", snap.Actions)
	}
	if snap.Actions[0].Summary != "edit auth.go" {
		t.Errorf("action summary = %q", snap.Actions[0].Summary)
	}
	if snap.GeneratedAt == 0 {
		t.Error("GeneratedAt not set")
	}
	if mp.calls != 1 {
		t.Errorf("provider called %d times, want 1", mp.calls)
	}
}

func TestGenerateCacheHitSkipsProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "do work"}}
	mp := &mockProvider{out: `{"purpose":"p","nextStep":"n"}`}

	if _, err := Generate(context.Background(), Options{Prov: mp, SessionPath: path, Messages: msgs}); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if mp.calls != 1 {
		t.Fatalf("calls = %d, want 1", mp.calls)
	}
	// Second call with the same message count and within staleMaxAge → cache hit.
	if _, err := Generate(context.Background(), Options{Prov: mp, SessionPath: path, Messages: msgs}); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if mp.calls != 1 {
		t.Errorf("calls = %d, want 1 (cache hit)", mp.calls)
	}
}

func TestGenerateCacheStaleOnNewMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	mp := &mockProvider{out: `{"purpose":"p"}`}
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "x"}}

	if _, err := Generate(context.Background(), Options{Prov: mp, SessionPath: path, Messages: msgs}); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	// Grow by staleMessageDelta → the cache is stale and Generate calls again.
	for i := 0; i < staleMessageDelta; i++ {
		msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Content: "more"})
	}
	if _, err := Generate(context.Background(), Options{Prov: mp, SessionPath: path, Messages: msgs}); err != nil {
		t.Fatalf("regen Generate: %v", err)
	}
	if mp.calls != 2 {
		t.Errorf("calls = %d, want 2 (stale regenerate)", mp.calls)
	}
}

func TestGenerateNoProviderErrors(t *testing.T) {
	if _, err := Generate(context.Background(), Options{Messages: []provider.Message{}}); err == nil {
		t.Error("want error when no provider is configured")
	}
}

func TestGenerateStreamError(t *testing.T) {
	mp := &mockProvider{err: errBoom}
	if _, err := Generate(context.Background(), Options{Prov: mp, Messages: []provider.Message{}}); err == nil {
		t.Error("want error on stream failure")
	}
}

func TestExtractCompactSummary(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "<compaction-summary>\nold stuff\n</compaction-summary>"},
		{Role: provider.RoleUser, Content: "recent"},
	}
	if got := extractCompactSummary(msgs); got != "old stuff" {
		t.Errorf("extractCompactSummary = %q", got)
	}
	if got := extractCompactSummary(nil); got != "" {
		t.Errorf("nil want empty, got %q", got)
	}
}

func TestRenderTranscriptMarksFailedTools(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "bash", Arguments: `{"command":"make"}`}}},
		{Role: provider.RoleTool, Name: "bash", Content: "error: build failed"},
	}
	out := renderTranscript(msgs)
	if !contains(out, "[FAILED]") {
		t.Errorf("expected [FAILED] marker, got:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
