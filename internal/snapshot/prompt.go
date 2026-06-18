package snapshot

// systemPrompt instructs the summarizer to emit a fixed-shape JSON snapshot.
// deadEnds is emphasized — surfacing abandoned approaches is the feature's reason
// for existing and the thing a heuristic snapshot cannot produce well.
const systemPrompt = `You generate a concise status snapshot for one task a coding agent is running, so the user can re-orient on it at a glance after switching away.

Read the task goal, any prior compaction summary, and the recent activity transcript. Output ONLY a JSON object with exactly these fields (use "" or [] when empty; never omit a field):

{
  "purpose": "what this task is trying to achieve — one line",
  "progress": "current progress, 1-2 lines",
  "actions": [{"kind":"tool","summary":"short description","failed":false}],
  "deadEnds": ["approaches tried that did NOT work, and why"],
  "nextStep": "the single most concrete next action"
}

Rules:
- Output ONLY the JSON object. No prose, no markdown fences, no commentary before or after.
- "deadEnds" is the most important field. Capture every approach that was tried and abandoned: failed tool calls (marked [FAILED] in the transcript), reverted edits, or solutions the agent discussed then dropped. Phrase each as "tried X -> didn't work because Y". Use [] only if there were genuinely none.
- "actions": the handful of most recent meaningful steps (tool calls and key assistant turns). Mark failed ones "failed": true.
- Be terse and factual. Do not invent anything not present in the transcript.`
