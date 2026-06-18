package main

import (
	"path/filepath"
	"strings"

	"reasonix/internal/control"
	"reasonix/internal/provider"
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
	TabID          string  `json:"tabId"`
	Title          string  `json:"title,omitempty"`
	Goal           string  `json:"goal,omitempty"`
	GoalStatus     string  `json:"goalStatus,omitempty"` // control.GoalStatus* (running/complete/blocked/stopped)
	RuntimeState   string  `json:"runtimeState"`         // running | waiting | idle | done | blocked
	CurrentStep    string  `json:"currentStep,omitempty"`
	Model          string  `json:"model,omitempty"`
	SessionPath    string  `json:"sessionPath,omitempty"`
	WorkspaceRoot  string  `json:"workspaceRoot,omitempty"`
	WorkspaceName  string  `json:"workspaceName,omitempty"`
	TopicTitle     string  `json:"topicTitle,omitempty"`
	Scope          string  `json:"scope,omitempty"`
	TurnCount      int     `json:"turnCount"`
	CreatedAt      int64   `json:"createdAt,omitempty"`
	LastActivityAt int64   `json:"lastActivityAt,omitempty"`
	CostUsd        float64 `json:"costUsd,omitempty"`
	CacheHit       int     `json:"cacheHit,omitempty"`
	CacheMiss      int     `json:"cacheMiss,omitempty"`
	Outcome        string  `json:"outcome,omitempty"` // "" active | "completed" | "abandoned"
	Active         bool    `json:"active"`
	Detached       bool    `json:"detached"`
	Historical     bool    `json:"historical"`
	Ready          bool    `json:"ready"`
	StartupErr     string  `json:"startupErr,omitempty"`
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
}

// MissionAction is one step in a task's recent activity, for the snapshot view.
type MissionAction struct {
	Kind    string `json:"kind"` // "tool" | "message"
	Summary string `json:"summary"`
	Failed  bool   `json:"failed,omitempty"`
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
	return out
}

// TaskSnapshot returns the expandable summary for one task. M1 derives it
// heuristically from the live session history; M2 replaces the body with an
// LLM call (internal/snapshot) returning the same shape.
func (a *App) TaskSnapshot(tabID string) (TaskSnapshot, error) {
	tab := a.tabByID(tabID)
	if tab == nil || tab.Ctrl == nil {
		return TaskSnapshot{TabID: tabID, GeneratedBy: "heuristic"}, nil
	}
	ctrl := tab.Ctrl
	return heuristicSnapshot(tabID, currentTabGoal(tab), currentTabGoalStatus(tab), ctrl.History()), nil
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
	} else {
		t.RuntimeState = "idle"
	}

	// Cost comes from the accumulated per-tab telemetry.
	tab.telemMu.Lock()
	t.CostUsd = tab.usageTelemetry.SessionCostUsd
	tab.telemMu.Unlock()

	if t.Title == "" {
		t.Title = missionFallbackTitle(t.Goal, t.SessionPath)
	}
	return t
}

func missionTaskFromSessionMeta(s SessionMeta) MissionTask {
	goal := strings.TrimSpace(s.Title)
	if goal == "" {
		goal = strings.TrimSpace(s.Preview) // first user message stands in for the goal
	}
	return MissionTask{
		TabID:          "hist:" + s.Path,
		Title:          goal,
		Goal:           goal,
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
