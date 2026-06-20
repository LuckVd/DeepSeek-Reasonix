import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown, ChevronRight, ExternalLink } from "lucide-react";
import { app } from "../lib/bridge";
import type { MissionTask, ModelInfo, TaskSnapshot } from "../lib/types";

// MissionCard is one task row on the Mission Control board. The collapsed head
// answers "what is it doing / does it need me" at a glance; the M3 action row
// lets the user approve, switch model, or close out the task without tabbing in;
// expanding pulls a TaskSnapshot (purpose/progress/actions/dead-ends/next-step)
// to re-orient after switching away.
//
// TODO(i18n): labels are inline Chinese for the prototype; migrate to t() later.

const STATE_LABEL: Record<MissionTask["runtimeState"], string> = {
  running: "运行中",
  waiting: "等你处理",
  idle: "空闲",
  done: "已完成",
  blocked: "已阻塞",
};

export function MissionCard({
  task,
  onOpenTab,
  onRefresh,
}: {
  task: MissionTask;
  onOpenTab?: (tabId: string) => void;
  onRefresh?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [snap, setSnap] = useState<TaskSnapshot | null>(null);
  const [loadingSnap, setLoadingSnap] = useState(false);
  const [models, setModels] = useState<ModelInfo[] | null>(null);
  const [busy, setBusy] = useState(false);

  const canExpand = !task.historical;
  const waiting = task.runtimeState === "waiting";
  const cost = task.costUsd ?? 0;
  const title = task.title || task.goal || "未命名任务";
  const archived = task.outcome === "completed" || task.outcome === "abandoned";

  async function toggle() {
    if (!canExpand) return;
    setOpen((v) => !v);
  }

  // Re-fetch the snapshot when the card opens or when the task has new activity
  // (the board refreshes on turn boundaries; the backend cache is warmed by then
  // via the stop trigger). TaskSnapshot is non-blocking (a cache read), so this
  // is cheap and never spins for an already-cached summary — it just picks up the
  // background refresh so an expanded card stays current instead of freezing on
  // the first-fetch value.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    (async () => {
      try {
        setLoadingSnap(true);
        const s = await app.TaskSnapshot(task.tabId);
        if (!cancelled) setSnap(s);
      } catch {
        /* keep last good snapshot */
      } finally {
        if (!cancelled) setLoadingSnap(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, task.tabId, task.lastActivityAt]);

  // Manual refresh: force a fresh LLM summary (bypasses the cache). Blocks on the
  // call — the user explicitly asked for an update.
  async function refreshManual() {
    if (loadingSnap || busy) return;
    try {
      setLoadingSnap(true);
      setSnap(await app.RefreshTaskSnapshot(task.tabId));
    } catch {
      /* keep last */
    } finally {
      setLoadingSnap(false);
    }
  }

  async function approve(id: string, allow: boolean) {
    if (busy) return;
    setBusy(true);
    try {
      await app.ApproveTab(task.tabId, id, allow, false, false);
      onRefresh?.();
    } finally {
      setBusy(false);
    }
  }

  async function markOutcome(outcome: string) {
    if (busy) return;
    setBusy(true);
    try {
      await app.SetTaskOutcome(task.tabId, outcome);
      onRefresh?.();
    } finally {
      setBusy(false);
    }
  }

  async function loadModels() {
    if (models) return;
    try {
      setModels(await app.ModelsForTab(task.tabId));
    } catch {
      /* leave models null; the select just shows no options */
    }
  }

  async function switchModel(ref: string) {
    if (busy || !ref) return;
    setBusy(true);
    try {
      await app.SetModelForTab(task.tabId, ref);
      onRefresh?.();
    } finally {
      setBusy(false);
    }
  }

  const approvals = task.pending?.filter((p) => p.kind === "approval") ?? [];
  const asks = task.pending?.filter((p) => p.kind === "ask") ?? [];

  return (
    <div
      className={[
        "mission-card",
        `mission-card--${task.runtimeState}`,
        waiting ? "mission-card--waiting" : "",
        task.historical ? "mission-card--historical" : "",
        task.detached ? "mission-card--detached" : "",
        archived ? "mission-card--archived" : "",
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <div
        className="mission-card__head"
        onClick={toggle}
        role={canExpand ? "button" : undefined}
        tabIndex={canExpand ? 0 : undefined}
      >
        <span className="mission-card__caret">
          {canExpand ? (open ? <ChevronDown size={15} /> : <ChevronRight size={15} />) : null}
        </span>
        <span className="mission-card__state">
          <span className="mission-card__dot" />
          {STATE_LABEL[task.runtimeState]}
        </span>
        <span className="mission-card__title" title={task.goal || title}>
          {title}
        </span>
        {task.currentStep && <span className="mission-card__step">{task.currentStep}</span>}
        <span className="mission-card__meta">
          {task.turnCount > 0 && <span className="mission-card__turns">{task.turnCount} 轮</span>}
          {task.model && <span className="mission-card__model">{task.model}</span>}
          {cost > 0 && <span className="mission-card__cost">${cost.toFixed(3)}</span>}
        </span>
      </div>

      {waiting && <div className="mission-card__hint">⚠ 这个任务正在等你确认 / 回答</div>}

      {/* M3: act on the task without tabbing into it. Historical tasks have none. */}
      {!task.historical && (
        <div className="mission-card__actions">
          {approvals.map((p) => (
            <div className="mission-pending" key={p.id}>
              <span className="mission-pending__text">
                批准 {p.tool}
                {p.subject ? ` · ${p.subject}` : ""}
              </span>
              <button className="mission-btn mission-btn--ok" onClick={() => approve(p.id, true)} disabled={busy}>
                批准
              </button>
              <button className="mission-btn mission-btn--no" onClick={() => approve(p.id, false)} disabled={busy}>
                拒绝
              </button>
            </div>
          ))}
          {asks.map((p) => (
            <div className="mission-pending" key={p.id}>
              <span className="mission-pending__text">回答 {p.subject}</span>
              {onOpenTab && (
                <button className="mission-btn" onClick={() => onOpenTab(task.tabId)}>
                  去回答
                </button>
              )}
            </div>
          ))}
          {!archived ? (
            <>
              <select
                className="mission-card__model-select"
                defaultValue=""
                onFocus={loadModels}
                onClick={loadModels}
                onChange={(e) => {
                  const v = e.target.value;
                  e.target.value = "";
                  void switchModel(v);
                }}
                disabled={busy}
              >
                <option value="" disabled>
                  换模型…
                </option>
                {models?.map((m) => (
                  <option key={m.ref} value={m.ref}>
                    {m.provider}/{m.model}
                  </option>
                ))}
              </select>
              <button className="mission-btn" onClick={() => markOutcome("completed")} disabled={busy}>
                完成
              </button>
              <button className="mission-btn" onClick={() => markOutcome("abandoned")} disabled={busy}>
                放弃
              </button>
            </>
          ) : (
            <button className="mission-btn" onClick={() => markOutcome("")} disabled={busy}>
              重新激活
            </button>
          )}
        </div>
      )}

      {open && (
        <div className="mission-card__body">
          {loadingSnap && <div className="mission-card__loading">正在生成摘要…</div>}
          {!loadingSnap && snap && <SnapshotView snap={snap} />}
          {!loadingSnap && !snap && <div className="mission-card__loading">暂无摘要</div>}
          {snap && (
            <div className="mission-card__snap-meta">
              <span className="mission-card__snap-status">{snapStatus(snap, task)}</span>
              <button
                className="mission-btn mission-btn--refresh-snap"
                onClick={refreshManual}
                disabled={loadingSnap || busy}
              >
                刷新摘要
              </button>
            </div>
          )}
          {canExpand && onOpenTab && (
            <button className="mission-card__open" onClick={() => onOpenTab(task.tabId)}>
              <ExternalLink size={13} /> 打开此任务
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// snapStatus says whether the displayed summary is current: "生成于 X 前" when it
// covers the latest activity, or a hint that newer activity exists (the background
// refresh will catch up; the manual button forces it immediately).
function snapStatus(snap: TaskSnapshot, task: MissionTask): string {
  if (!snap.generatedAt) return "";
  if (task.lastActivityAt && task.lastActivityAt > snap.generatedAt) {
    return "有新活动，摘要更新中…";
  }
  return `生成于 ${relTime(snap.generatedAt)}`;
}

function relTime(unixSec: number): string {
  const diff = Math.max(0, Math.floor(Date.now() / 1000) - unixSec);
  if (diff < 60) return "刚刚";
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

function SnapshotView({ snap }: { snap: TaskSnapshot }) {
  return (
    <div className="mission-snap">
      {snap.purpose && <Section label="目的">{snap.purpose}</Section>}
      {snap.progress && <Section label="进度">{snap.progress}</Section>}
      {snap.actions && snap.actions.length > 0 && (
        <Section label="已做的动作">
          <ul className="mission-snap__list">
            {snap.actions.map((a, i) => (
              <li key={i} className={a.failed ? "mission-snap__item--failed" : ""}>
                {a.failed ? "✗ " : "• "}
                {a.summary}
              </li>
            ))}
          </ul>
        </Section>
      )}
      {snap.deadEnds && snap.deadEnds.length > 0 && (
        <Section label="走过的弯路(走不通)" emphasis>
          <ul className="mission-snap__list mission-snap__list--deadends">
            {snap.deadEnds.map((d, i) => (
              <li key={i}>🚫 {d}</li>
            ))}
          </ul>
        </Section>
      )}
      {snap.nextStep && <Section label="下一步">{snap.nextStep}</Section>}
    </div>
  );
}

function Section({
  label,
  emphasis,
  children,
}: {
  label: string;
  emphasis?: boolean;
  children: ReactNode;
}) {
  return (
    <div className={`mission-snap__section${emphasis ? " mission-snap__section--emphasis" : ""}`}>
      <div className="mission-snap__label">{label}</div>
      <div className="mission-snap__value">{children}</div>
    </div>
  );
}
