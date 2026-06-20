package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/provider"
	"reasonix/internal/snapshot"
)

// Mission Control — a read-mostly aggregate view of every task the user is
// running (or has run) across desktop tabs. The board exists so a user juggling
// several concurrent tasks can, at a glance, see what each one is doing, which
// need their attention, and what they cost — without tabbing into each one and
// losing the thread of the others. This file holds only the aggregate + the
// heuristic snapshot; live task actions (approve / switch model / cancel) reuse
// the existing tab-scoped App methods, and the LLM snapshot lands in M2.

// MissionTask is one row on the board. Active and background-detached tabs
// carry live runtime state; historical sessions (no live controller) carry only
// persisted metadata so the user can recall what they ran before.
type MissionTask struct {
	TabID          string           `json:"tabId"`
	Title          string           `json:"title,omitempty"`
	Goal           string           `json:"goal,omitempty"`
	Purpose        string           `json:"purpose,omitempty"`    // one-line "what is it doing": cached snapshot purpose, else goal
	Progress       string           `json:"progress,omitempty"`   // cached snapshot progress (where it's at) — shown on the collapsed card
	NextStep       string           `json:"nextStep,omitempty"`   // cached snapshot nextStep (what's next) — shown on the collapsed card
	GoalStatus     string           `json:"goalStatus,omitempty"` // control.GoalStatus* (running/complete/blocked/stopped)
	RuntimeState   string           `json:"runtimeState"`         // running | waiting | idle | done | blocked
	CurrentStep    string           `json:"currentStep,omitempty"`
	Model          string           `json:"model,omitempty"`
	SessionPath    string           `json:"sessionPath,omitempty"`
	WorkspaceRoot  string           `json:"workspaceRoot,omitempty"`
	WorkspaceName  string           `json:"workspaceName,omitempty"`
	TopicTitle     string           `json:"topicTitle,omitempty"`
	Scope          string           `json:"scope,omitempty"`
	TurnCount      int              `json:"turnCount"`
	CreatedAt      int64            `json:"createdAt,omitempty"`
	LastActivityAt int64            `json:"lastActivityAt,omitempty"`
	CostUsd        float64          `json:"costUsd,omitempty"`
	CacheHit       int              `json:"cacheHit,omitempty"`
	CacheMiss      int              `json:"cacheMiss,omitempty"`
	Outcome        string           `json:"outcome,omitempty"` // "" active | "completed" | "abandoned"
	Pending        []MissionPending `json:"pending,omitempty"` // approvals/asks awaiting the user
	Active         bool             `json:"active"`
	Detached       bool             `json:"detached"`
	Historical     bool             `json:"historical"`
	Ready          bool             `json:"ready"`
	StartupErr     string           `json:"startupErr,omitempty"`
}

// TaskSnapshot is the expandable summary behind one card: the purpose, the
// progress, the actions tried, the dead-ends (paths attempted that didn't
// work), and the next step. The dead-ends field is the differentiator — it
// directly attacks "I forget what this agent already tried and what failed".
//
// M1 derives this heuristically from the session history; M2 swaps the
// generator for an LLM call (internal/snapshot) while keeping this shape, so
// the frontend needs no changes between the two.
type TaskSnapshot struct {
	TabID       string          `json:"tabId"`
	Purpose     string          `json:"purpose,omitempty"`
	Progress    string          `json:"progress,omitempty"`
	NextStep    string          `json:"nextStep,omitempty"`
	Actions     []MissionAction `json:"actions,omitempty"`
	DeadEnds    []string        `json:"deadEnds,omitempty"`
	GeneratedBy string          `json:"generatedBy"` // "heuristic" (M1) | "llm" (M2)
	GeneratedAt int64           `json:"generatedAt"` // unix seconds — when this summary was produced (for "X ago" + staleness)
}

// MissionAction is one step in a task's recent activity, for the snapshot view.
type MissionAction struct {
	Kind    string `json:"kind"` // "tool" | "message"
	Summary string `json:"summary"`
	Failed  bool   `json:"failed,omitempty"`
}

// MissionPending is one prompt awaiting the user (a tool approval or an ask
// question), so the board can show "needs you" and act on it without tabbing in.
type MissionPending struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "approval" | "ask"
	Tool    string `json:"tool,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Task outcomes set by the user from the board (M3). Kept distinct from the
// agent-driven GoalStatus so a user can mark a task done/abandoned regardless
// of what the model last reported.
const (
	TaskOutcomeCompleted = "completed"
	TaskOutcomeAbandoned = "abandoned"
)

const missionMaxActions = 8

// MissionTasks returns every task for the board. Live tabs (the visible ones
// plus background-detached runtimes) come first with full runtime state;
// historical sessions from the active workspace follow so the user can recall
// prior runs.
func (a *App) MissionTasks() []MissionTask {
	a.mu.RLock()
	tabs := a.runtimeTabsLocked()
	activeID := a.activeTabID
	detached := make(map[string]bool, len(a.detachedSessions))
	for _, t := range a.detachedSessions {
		if t != nil {
			detached[t.ID] = true
		}
	}
	openPaths := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		if key := sessionRuntimeKey(t.currentSessionPath()); key != "" {
			openPaths[key] = true
		}
	}
	a.mu.RUnlock()

	out := make([]MissionTask, 0, len(tabs)+8)
	for _, tab := range tabs {
		if tab == nil {
			continue
		}
		out = append(out, missionTaskFromTab(tab, tab.ID == activeID, detached[tab.ID]))
	}

	// Historical sessions from the active workspace — those not currently open.
	for _, s := range a.ListSessions() {
		if s.DeletedAt > 0 || s.Current || s.Open {
			continue
		}
		if openPaths[sessionRuntimeKey(s.Path)] {
			continue
		}
		out = append(out, missionTaskFromSessionMeta(s))
	}
	// Snapshots are generated on demand (TaskSnapshot on expand,
	// RefreshTaskSnapshot on the manual button) — see kickSnapshot. The board no
	// longer pre-warms them: the real-time head (runtimeState / pending /
	// currentStep) answers "what is it doing / does it need me" at a glance
	// without an LLM, so there is no need to summarize tabs the user never opens.
	return out
}

// kickSnapshot launches a background snapshot generation for one tab unless one
// is already in flight for it (dedup across on-demand expands). It copies the
// history so the goroutine reads a stable snapshot while the agent may keep
// appending.
func (a *App) kickSnapshot(tabID, sessionPath, goal string, msgs []provider.Message) {
	a.snapshotGenMu.Lock()
	if a.snapshotInflight == nil {
		a.snapshotInflight = make(map[string]bool)
	}
	if a.snapshotInflight[tabID] {
		a.snapshotGenMu.Unlock()
		return
	}
	a.snapshotInflight[tabID] = true
	a.snapshotGenMu.Unlock()
	hist := append([]provider.Message(nil), msgs...)
	go a.generateSnapshotBackground(tabID, sessionPath, goal, hist)
}

// generateSnapshotBackground fills the snapshot cache for one tab (incrementally)
// and clears the inflight flag. The result is intentionally discarded — the
// board reads it back from the cache on the next view or refresh.
func (a *App) generateSnapshotBackground(tabID, sessionPath, goal string, msgs []provider.Message) {
	defer func() {
		a.snapshotGenMu.Lock()
		delete(a.snapshotInflight, tabID)
		a.snapshotGenMu.Unlock()
	}()
	prov, pricing := a.snapshotProvider()
	if prov == nil {
		return
	}
	_, _ = snapshot.Generate(a.bootContext(), snapshot.Options{
		SessionPath: sessionPath,
		Messages:    msgs,
		Goal:        goal,
		Prov:        prov,
		Pricing:     pricing,
	})
}

// TaskSnapshot returns the expandable summary for one task. It asks the snapshot
// package for an LLM summary (a cheap model, independent of the task's own
// model) and falls back to the M1 heuristic when no provider is configured or
// the call fails — so a card always has something to show. Historical tasks
// (id "hist:<sessionPath>") load the session file and summarize that.
func (a *App) TaskSnapshot(tabID string) (TaskSnapshot, error) {
	if histPath, ok := strings.CutPrefix(tabID, "hist:"); ok {
		if sess, err := agent.LoadSession(histPath); err == nil {
			return a.llmOrHeuristicSnapshot(tabID, histPath, "", "", sess.Snapshot()), nil
		}
		return TaskSnapshot{TabID: tabID, GeneratedBy: "heuristic"}, nil
	}
	tab := a.tabByID(tabID)
	if tab == nil || tab.Ctrl == nil {
		return TaskSnapshot{TabID: tabID, GeneratedBy: "heuristic"}, nil
	}
	ctrl := tab.Ctrl
	sessionPath := ctrl.SessionPath()
	goal := currentTabGoal(tab)
	goalStatus := currentTabGoalStatus(tab)
	hist := ctrl.History()

	// Non-blocking (stale-while-revalidate): the last cached snapshot shows
	// instantly, so expanding a card — even a task still running — never waits on
	// an LLM call. The cache is filled on demand here (kickSnapshot) and by the
	// manual "refresh summary" button; there is no board-wide pre-warm. We only
	// kick a background fill when nothing is cached yet AND the task isn't
	// mid-turn; a running task with no cache shows the heuristic until it stops
	// and gets summarized.
	if snap := snapshot.Load(sessionPath); snap != nil {
		return toTaskSnapshot(tabID, snap, "llm"), nil
	}
	if !ctrl.RuntimeStatus().Running {
		a.kickSnapshot(tabID, sessionPath, goal, hist)
	}
	return heuristicSnapshot(tabID, goal, goalStatus, hist), nil
}

// RefreshTaskSnapshot forces a fresh snapshot for one task (bypassing the cache)
// and returns it. It is the board's manual "refresh summary" action — unlike
// TaskSnapshot it blocks on the LLM call, because the user explicitly asked for
// an update. Historical tasks ("hist:<sessionPath>") summarize that session file.
func (a *App) RefreshTaskSnapshot(tabID string) (TaskSnapshot, error) {
	if histPath, ok := strings.CutPrefix(tabID, "hist:"); ok {
		if sess, err := agent.LoadSession(histPath); err == nil {
			return a.llmOrHeuristicSnapshot(tabID, histPath, "", "", sess.Snapshot()), nil
		}
		return TaskSnapshot{TabID: tabID, GeneratedBy: "heuristic", GeneratedAt: time.Now().Unix()}, nil
	}
	tab := a.tabByID(tabID)
	if tab == nil || tab.Ctrl == nil {
		return TaskSnapshot{TabID: tabID, GeneratedBy: "heuristic", GeneratedAt: time.Now().Unix()}, nil
	}
	ctrl := tab.Ctrl
	prov, pricing := a.snapshotProvider()
	if prov != nil {
		snap, err := snapshot.Generate(a.bootContext(), snapshot.Options{
			SessionPath: ctrl.SessionPath(),
			Messages:    ctrl.History(),
			Goal:        currentTabGoal(tab),
			Prov:        prov,
			Pricing:     pricing,
			Force:       true,
		})
		if err == nil {
			return toTaskSnapshot(tabID, snap, "llm"), nil
		}
	}
	return heuristicSnapshot(tabID, currentTabGoal(tab), currentTabGoalStatus(tab), ctrl.History()), nil
}

// llmOrHeuristicSnapshot generates an LLM snapshot and degrades to the M1
// heuristic on any failure (no provider, timeout, empty/garbage output).
func (a *App) llmOrHeuristicSnapshot(tabID, sessionPath, goal, goalStatus string, msgs []provider.Message) TaskSnapshot {
	prov, pricing := a.snapshotProvider()
	if prov != nil {
		snap, err := snapshot.Generate(a.bootContext(), snapshot.Options{
			SessionPath: sessionPath,
			Messages:    msgs,
			Goal:        goal,
			Prov:        prov,
			Pricing:     pricing,
		})
		if err == nil {
			return toTaskSnapshot(tabID, snap, "llm")
		}
	}
	return heuristicSnapshot(tabID, goal, goalStatus, msgs)
}

// toTaskSnapshot maps the snapshot package's Snapshot onto the board's
// TaskSnapshot — the same shape the frontend already renders from M1.
func toTaskSnapshot(tabID string, snap *snapshot.Snapshot, generatedBy string) TaskSnapshot {
	t := TaskSnapshot{
		TabID:       tabID,
		Purpose:     snap.Purpose,
		Progress:    snap.Progress,
		NextStep:    snap.NextStep,
		DeadEnds:    snap.DeadEnds,
		GeneratedBy: generatedBy,
		GeneratedAt: snap.GeneratedAt,
	}
	if len(snap.Actions) > 0 {
		t.Actions = make([]MissionAction, len(snap.Actions))
		for i, act := range snap.Actions {
			t.Actions[i] = MissionAction{Kind: act.Kind, Summary: act.Summary, Failed: act.Failed}
		}
	}
	return t
}

// snapshotProvider lazily builds the cheap model used for task snapshots —
// mirrors serve.initTitleProvider: SnapshotModel → SubagentModel → the default
// model. Returns nil if nothing resolves; callers fall back to the heuristic.
func (a *App) snapshotProvider() (provider.Provider, *provider.Pricing) {
	a.snapshotProvMu.Lock()
	defer a.snapshotProvMu.Unlock()
	if a.snapshotProv != nil {
		return a.snapshotProv, a.snapshotPrice
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil
	}
	ref := strings.TrimSpace(cfg.Agent.SnapshotModel)
	if ref == "" {
		ref = strings.TrimSpace(cfg.Agent.SubagentModel)
	}
	resolved, _, ok := cfg.ResolveModelWithFallback(ref)
	if !ok {
		return nil, nil
	}
	entry, ok := cfg.ResolveModel(resolved)
	if !ok {
		return nil, nil
	}
	prov, err := boot.NewProvider(entry)
	if err != nil {
		return nil, nil
	}
	a.snapshotProv = prov
	a.snapshotPrice = entry.Price
	return prov, entry.Price
}

// missionPendingFromCtrl maps the controller's pending approvals/asks onto the
// board's wire shape.
func missionPendingFromCtrl(ctrl *control.Controller) []MissionPending {
	if ctrl == nil {
		return nil
	}
	items := append(ctrl.PendingApprovals(), ctrl.PendingAsks()...)
	if len(items) == 0 {
		return nil
	}
	out := make([]MissionPending, len(items))
	for i, p := range items {
		out[i] = MissionPending{ID: p.ID, Kind: p.Kind, Tool: p.Tool, Subject: p.Subject}
	}
	return out
}

// SetTaskOutcome marks a task completed or abandoned from the board (M3). This
// is the user's call, orthogonal to the agent's own GoalStatus — it does not
// stop a running turn, it just records the outcome (and persists it) so the task
// sorts into the archive. An empty outcome reactivates the task.
func (a *App) SetTaskOutcome(tabID, outcome string) (bool, error) {
	outcome = strings.TrimSpace(outcome)
	switch outcome {
	case "", TaskOutcomeCompleted, TaskOutcomeAbandoned:
	default:
		return false, fmt.Errorf("invalid task outcome %q", outcome)
	}
	a.mu.Lock()
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		a.mu.Unlock()
		return false, fmt.Errorf("task %q not found", tabID)
	}
	tab.taskOutcome = outcome
	tabIDForSave := tab.ID
	a.mu.Unlock()
	a.mu.Lock()
	if a.tabs[tabIDForSave] == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	return true, nil
}

func missionTaskFromTab(tab *WorkspaceTab, active, detached bool) MissionTask {
	t := MissionTask{
		TabID:         tab.ID,
		Title:         tab.TopicTitle,
		Goal:          currentTabGoal(tab),
		GoalStatus:    currentTabGoalStatus(tab),
		Model:         tab.Label,
		SessionPath:   tab.currentSessionPath(),
		WorkspaceRoot: tab.WorkspaceRoot,
		TopicTitle:    tab.TopicTitle,
		Scope:         tab.Scope,
		Active:        active,
		Detached:      detached,
		Ready:         tab.Ready,
		StartupErr:    tab.StartupErr,
	}
	t.WorkspaceName = tabWorkspaceName(tab, tab.WorkspaceRoot)

	if ctrl := tab.Ctrl; ctrl != nil {
		rs := ctrl.RuntimeStatus()
		t.TurnCount = ctrl.Turn()
		hit, miss := ctrl.SessionCache()
		t.CacheHit, t.CacheMiss = hit, miss
		t.RuntimeState = missionRuntimeState(rs, t.GoalStatus)
		t.CurrentStep = strings.TrimSpace(tab.ActivityStatus)
		if t.CurrentStep == "" {
			t.CurrentStep = lastActivitySummary(ctrl.History())
		}
		t.Pending = missionPendingFromCtrl(ctrl)
	} else {
		t.RuntimeState = "idle"
	}
	t.Outcome = tab.taskOutcome

	// Cost comes from the accumulated per-tab telemetry.
	tab.telemMu.Lock()
	t.CostUsd = tab.usageTelemetry.SessionCostUsd
	tab.telemMu.Unlock()

	if t.Title == "" {
		t.Title = missionFallbackTitle(t.Goal, t.SessionPath)
	}
	// Purpose/progress/nextStep from the cached snapshot if available (free sidecar
	// read), else the goal. Read-only — never triggers generation.
	t.Purpose, t.Progress, t.NextStep = missionCachedSummary(t.SessionPath, t.Goal)
	return t
}

func missionTaskFromSessionMeta(s SessionMeta) MissionTask {
	goal := strings.TrimSpace(s.Title)
	if goal == "" {
		goal = strings.TrimSpace(s.Preview) // first user message stands in for the goal
	}
	purpose, progress, nextStep := missionCachedSummary(s.Path, goal)
	return MissionTask{
		TabID:          "hist:" + s.Path,
		Title:          goal,
		Goal:           goal,
		Purpose:        purpose,
		Progress:       progress,
		NextStep:       nextStep,
		SessionPath:    s.Path,
		WorkspaceRoot:  s.WorkspaceRoot,
		TopicTitle:     s.TopicTitle,
		Scope:          s.Scope,
		TurnCount:      s.Turns,
		CreatedAt:      s.CreatedAt,
		LastActivityAt: s.LastActivityAt,
		RuntimeState:   "idle",
		Historical:     true,
	}
}

// missionRuntimeState collapses the controller's runtime flags plus the
// agent-reported goal status into the coarse state the board displays. A
// pending approval/ask wins ("waiting") so the attention layer can surface it.
func missionRuntimeState(rs control.RuntimeStatus, goalStatus string) string {
	if rs.PendingPrompt {
		return "waiting"
	}
	if rs.Running {
		return "running"
	}
	switch goalStatus {
	case control.GoalStatusComplete:
		return "done"
	case control.GoalStatusBlocked:
		return "blocked"
	}
	return "idle"
}

// missionCachedSummary reads the cached LLM snapshot (a free sidecar read — no
// provider, no generation) and returns its purpose/progress/nextStep, so the
// collapsed card can show the summary's key lines without expanding and without
// paying for on-demand generation. Purpose falls back to the goal when no
// snapshot exists; progress/nextStep are empty then.
func missionCachedSummary(sessionPath, goal string) (purpose, progress, nextStep string) {
	purpose = goal
	if sessionPath == "" {
		return
	}
	if snap := snapshot.Load(sessionPath); snap != nil {
		if p := strings.TrimSpace(snap.Purpose); p != "" {
			purpose = p
		}
		progress = strings.TrimSpace(snap.Progress)
		nextStep = strings.TrimSpace(snap.NextStep)
	}
	return
}

// lastActivitySummary is the M1 "what is it doing right now" hint, derived
// from the most recent assistant action (last tool call, else last answer
// text). M2's LLM snapshot replaces this with a real progress sentence.
func lastActivitySummary(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			tc := m.ToolCalls[len(m.ToolCalls)-1]
			return strings.TrimSpace(tc.Name + missionArgsHint(tc.Arguments))
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == provider.RoleAssistant {
			if s := strings.TrimSpace(m.Content); s != "" {
				return truncateForDisplay(s, 80)
			}
		}
	}
	return ""
}

func heuristicSnapshot(tabID, goal, goalStatus string, msgs []provider.Message) TaskSnapshot {
	snap := TaskSnapshot{
		TabID:       tabID,
		Purpose:     goal,
		Progress:    missionProgressText(goalStatus),
		GeneratedBy: "heuristic",
		GeneratedAt: time.Now().Unix(),
	}

	// Fall back to the first user message when no explicit goal is set.
	if strings.TrimSpace(snap.Purpose) == "" {
		for _, m := range msgs {
			if m.Role == provider.RoleUser {
				if s := strings.TrimSpace(m.Content); s != "" {
					snap.Purpose = truncateForDisplay(s, 140)
				}
				break
			}
		}
	}

	// A tool result beginning with error:/blocked: marks its call a dead-end.
	failed := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == provider.RoleTool && isMissionToolError(m.Content) && m.ToolCallID != "" {
			failed[m.ToolCallID] = true
		}
	}

	var actions []MissionAction
	for _, m := range msgs {
		if m.Role != provider.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			actions = append(actions, MissionAction{
				Kind:    "tool",
				Summary: strings.TrimSpace(tc.Name + missionArgsHint(tc.Arguments)),
				Failed:  failed[tc.ID],
			})
		}
	}
	if n := len(actions); n > missionMaxActions {
		actions = actions[n-missionMaxActions:]
	}
	snap.Actions = actions
	for _, a := range actions {
		if a.Failed {
			snap.DeadEnds = append(snap.DeadEnds, a.Summary+" → failed")
		}
	}

	// nextStep: the most recent assistant answer text.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == provider.RoleAssistant {
			if s := strings.TrimSpace(m.Content); s != "" {
				snap.NextStep = truncateForDisplay(s, 160)
				break
			}
		}
	}
	return snap
}

func missionProgressText(goalStatus string) string {
	switch goalStatus {
	case control.GoalStatusRunning:
		return "in progress"
	case control.GoalStatusComplete:
		return "complete"
	case control.GoalStatusBlocked:
		return "blocked"
	default:
		return "idle"
	}
}

// isMissionToolError mirrors compact.isToolError: a tool result that signals
// failure. Such calls feed the snapshot's dead-ends.
func isMissionToolError(content string) bool {
	s := strings.ToLower(strings.TrimSpace(content))
	return strings.HasPrefix(s, "error:") ||
		strings.HasPrefix(s, "blocked:") ||
		strings.Contains(s, "permission denied")
}

// missionArgsHint pulls a short, human-useful identifier (a path, command,
// pattern, …) out of a tool call's raw JSON arguments so the board can show
// "edit auth.go" rather than just "edit". Best-effort string scan; the args are
// model-produced JSON already validated by the time they land here.
func missionArgsHint(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	for _, key := range []string{`"file_path"`, `"path"`, `"command"`, `"pattern"`, `"query"`, `"url"`, `"note"`} {
		if v := missionExtractJSONString(args, key); v != "" {
			return " " + truncateForDisplay(v, 60)
		}
	}
	return " " + truncateForDisplay(args, 40)
}

func missionExtractJSONString(s, key string) string {
	idx := strings.Index(s, key)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(key):]
	rest = strings.TrimLeft(rest, " :\"\t")
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		return rest[:end]
	}
	return ""
}

func missionFallbackTitle(goal, sessionPath string) string {
	if g := strings.TrimSpace(goal); g != "" {
		return truncateForDisplay(g, 60)
	}
	if sessionPath != "" {
		return filepath.Base(sessionPath)
	}
	return "Untitled task"
}

// truncateForDisplay caps a string to n runes, appending an ellipsis — rune-
// safe so CJK text isn't split mid-character.
func truncateForDisplay(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
