// Package snapshot generates a concise, human-readable status snapshot for one
// task: purpose / progress / actions tried / dead-ends (paths that didn't work)
// / next step. It is the expandable summary behind each Mission Control card.
//
// The snapshot is produced by a single independent LLM call (a cheap model such
// as deepseek-flash) over the task's session history — it is NOT folded into the
// agent's main conversation, so it never disturbs the cache-stable prefix. Calls
// are pull-based (the board invokes Generate on demand) and memoized in a sidecar
// file keyed by message count, so a task viewed repeatedly costs at most one call
// per chunk of new activity.
//
// Dead-ends are the point of the feature: where M1's heuristic could only catch
// tool calls that errored, the LLM here also surfaces approaches discussed and
// then dropped. On any failure (no provider, timeout, empty/garbage output) the
// caller degrades to the M1 heuristic — Generate just returns an error.
package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

// Snapshot is one task's status summary. Field shape matches the board's
// TaskSnapshot so the frontend renders it unchanged from M1.
type Snapshot struct {
	Purpose  string   `json:"purpose,omitempty"`
	Progress string   `json:"progress,omitempty"`
	NextStep string   `json:"nextStep,omitempty"`
	Actions  []Action `json:"actions,omitempty"`
	DeadEnds []string `json:"deadEnds,omitempty"`
	// GeneratedAt is a unix timestamp set by Generate after a successful call.
	GeneratedAt int64 `json:"generatedAt"`
}

// Action is one step in the recent-activity list.
type Action struct {
	Kind    string `json:"kind"` // "tool" | "message"
	Summary string `json:"summary"`
	Failed  bool   `json:"failed,omitempty"`
}

// Options configures one Generate call. The caller builds Prov/Pricing/Sink
// (mirroring how the desktop wires its title provider); snapshot stays free of
// config/boot dependencies.
type Options struct {
	SessionPath string             // for the sidecar cache; "" disables caching
	Messages    []provider.Message // the task's session history
	Goal        string             // explicit task goal if known (improves "purpose")
	Prov        provider.Provider  // required — the cheap summarizer model
	Pricing     *provider.Pricing  // optional, for usage cost attribution
	Sink        event.Sink         // optional, emits a Usage event for accounting
	MaxTokens   int                // default defaultMaxTokens
}

const (
	snapshotTimeout  = 60 * time.Second
	defaultMaxTokens = 600
)

// Generate produces a snapshot, reading from / writing to the sidecar cache when
// SessionPath is set. It returns an error on any failure; callers should fall
// back to a heuristic summary rather than surface this to the user.
func Generate(ctx context.Context, opts Options) (*Snapshot, error) {
	if opts.Prov == nil {
		return nil, errors.New("snapshot: provider not configured")
	}
	if cached, ok := loadCache(opts.SessionPath, opts.Messages); ok {
		return cached, nil
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	sys, user := buildPrompt(opts.Messages, opts.Goal)

	text, err := stream(ctx, opts, sys, user, maxTokens)
	if err != nil {
		return nil, err
	}
	snap, err := parseSnapshot(text)
	if err != nil {
		return nil, err
	}
	snap.GeneratedAt = time.Now().Unix()
	saveCache(opts.SessionPath, opts.Messages, snap)
	return snap, nil
}

// stream runs one completion and accumulates the text, mirroring
// agent.summarize's select loop so a stalled stream still unblocks on timeout.
// Any usage chunk is emitted as a Usage event (source = snapshot).
func stream(ctx context.Context, opts Options, sys, user string, maxTokens int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	ch, err := opts.Prov.Stream(ctx, provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: sys},
			{Role: provider.RoleUser, Content: user},
		},
		Temperature: 0.3,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var usage *provider.Usage
	emitUsage := func() {
		if usage != nil && usage.TotalTokens > 0 && !nilutil.IsNil(opts.Sink) {
			opts.Sink.Emit(event.Event{Kind: event.Usage, Usage: usage, Pricing: opts.Pricing, UsageSource: event.UsageSourceSnapshot})
		}
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				emitUsage()
				s := strings.TrimSpace(b.String())
				if s == "" {
					return "", errors.New("snapshot: empty output")
				}
				return s, nil
			}
			switch chunk.Type {
			case provider.ChunkText:
				b.WriteString(chunk.Text)
			case provider.ChunkUsage:
				usage = chunk.Usage
			case provider.ChunkError:
				return "", chunk.Err
			}
		}
	}
}

// parseSnapshot extracts the JSON object the model was asked to emit. It tolerates
// a ```json fence and any surrounding prose, then slices to the outermost braces.
func parseSnapshot(text string) (*Snapshot, error) {
	text = strings.TrimSpace(stripFence(text))
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("snapshot: no JSON object in output")
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(text[start:end+1]), &snap); err != nil {
		return nil, fmt.Errorf("snapshot: parse JSON: %w", err)
	}
	return &snap, nil
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// buildPrompt assembles the system + user messages: goal (if known), the most
// recent compaction summary (so a long task costs bounded input), and a recent
// transcript with failures marked.
func buildPrompt(msgs []provider.Message, goal string) (sys, user string) {
	var b strings.Builder
	if g := strings.TrimSpace(goal); g != "" {
		fmt.Fprintf(&b, "Task goal: %s\n\n", g)
	}
	if summary := extractCompactSummary(msgs); summary != "" {
		fmt.Fprintf(&b, "Prior compaction summary (what happened earlier):\n%s\n\n", summary)
	}
	b.WriteString("Recent activity (tool results that failed are marked [FAILED]):\n")
	b.WriteString(renderTranscript(msgs))
	return systemPrompt, b.String()
}

// extractCompactSummary returns the most recent <compaction-summary>… block, if
// any — the agent folds old turns into one of these when the context window
// fills. Reusing it keeps the snapshot's input bounded for long tasks.
func extractCompactSummary(msgs []provider.Message) string {
	const open = "<compaction-summary>"
	const close = "</compaction-summary>"
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != provider.RoleUser {
			continue
		}
		c := strings.TrimLeft(m.Content, "\n ")
		if !strings.HasPrefix(c, open) {
			continue
		}
		body := strings.TrimPrefix(c, open)
		if j := strings.Index(body, close); j >= 0 {
			body = body[:j]
		}
		return strings.TrimSpace(body)
	}
	return ""
}

// renderTranscript flattens the most recent messages into a compact transcript,
// skipping the compaction-summary pseudo-message (already surfaced above) and
// clamping each entry so the input stays small.
func renderTranscript(msgs []provider.Message) string {
	const maxMessages = 40
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			if strings.HasPrefix(strings.TrimLeft(m.Content, "\n "), "<compaction-summary>") {
				continue
			}
			fmt.Fprintf(&b, "[user] %s\n", oneline(m.Content, 300))
		case provider.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", oneline(m.Content, 300))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[tool call] %s %s\n", tc.Name, oneline(tc.Arguments, 120))
			}
		case provider.RoleTool:
			tag := ""
			if isToolError(m.Content) {
				tag = " [FAILED]"
			}
			fmt.Fprintf(&b, "[tool %s result]%s %s\n", m.Name, tag, oneline(m.Content, 200))
		}
	}
	return b.String()
}

// isToolError mirrors compact.isToolError: a tool result that signals failure.
// Such results are the most direct dead-end signal fed to the summarizer.
func isToolError(content string) bool {
	s := strings.ToLower(strings.TrimSpace(content))
	return strings.HasPrefix(s, "error:") ||
		strings.HasPrefix(s, "blocked:") ||
		strings.Contains(s, "permission denied")
}

// oneline collapses whitespace and caps to n runes so a single verbose message
// can't dominate the (token-budgeted) prompt.
func oneline(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
