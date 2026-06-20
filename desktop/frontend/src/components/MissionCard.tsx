import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown, ChevronRight, ExternalLink, MoreHorizontal } from "lucide-react";
import { app } from "../lib/bridge";
import type { MissionTask, TaskSnapshot } from "../lib/types";

// MissionCard is one task tile on the Mission Control board. Four sections:
//   1. top bar — status badge + model tag + project + caret
//   2. title   — what the task is (the cached snapshot purpose)
//   3. detail  — 进度 | 下一步, two labeled columns (visible without expanding)
//   4. foot    — time + hover-revealed actions (open / ⋯ 完成·放弃)
// Expanding reveals only the dead-ends. It's a summary, not a timeline. The
// palette is the board's own light, semantic set (see .mission-panel tokens).
//
// TODO(i18n): labels are inline Chinese for the prototype; migrate to t() later.

const STATE_LABEL: Record<MissionTask["runtimeState"], string> = {
  running: "执行中",
  waiting: "待你处理",
  idle: "等你指令",
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
  const [busy, setBusy] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);

  const canExpand = !task.historical;
  const waiting = task.runtimeState === "waiting";
  const cost = task.costUsd ?? 0;
  const title = task.title || task.goal || "未命名任务";
  const archived = task.outcome === "completed" || task.outcome === "abandoned";
  const purpose = task.purpose || task.goal || title;
  const label = task.historical ? "历史" : STATE_LABEL[task.runtimeState];

  // 进度 falls back to the last live action when no cached summary exists, so the
  // column is never empty; 下一步 only shows when the snapshot has one.
  const liveStatus = task.currentStep
    ? task.currentStep
    : task.lastActivityAt
      ? relTime(task.lastActivityAt)
      : "";
  const progressText = task.progress || (waiting ? "" : liveStatus) || "—";
  const nextStepText = task.nextStep || "—";

  const footTime = task.lastActivityAt
    ? relTime(task.lastActivityAt)
    : task.createdAt
      ? relTime(task.createdAt)
      : "";

  function toggle() {
    if (canExpand) setOpen((v) => !v);
  }

  // Pull the snapshot when the card opens (cache-first, non-blocking) — feeds the
  // dead-ends detail and the manual refresh only.
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

  const approvals = task.pending?.filter((p) => p.kind === "approval") ?? [];
  const asks = task.pending?.filter((p) => p.kind === "ask") ?? [];
  const hasPending = approvals.length > 0 || asks.length > 0;

  return (
    <div
      className={[
        "mission-card",
        `mission-card--${task.runtimeState}`,
        task.historical ? "mission-card--historical" : "",
        task.detached ? "mission-card--detached" : "",
        archived ? "mission-card--archived" : "",
        menuOpen ? "mission-card--menu-open" : "",
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {/* Sections 1–3 are the clickable glance surface (click toggles dead-ends). */}
      <div
        className="mission-card__main"
        onClick={toggle}
        role={canExpand ? "button" : undefined}
        tabIndex={canExpand ? 0 : undefined}
      >
        <div className="mission-card__bar">
          <span className="mission-card__badge">
            <span className="mission-card__badge-dot" />
            {label}
          </span>
          {task.model && <span className="mission-card__model">{task.model}</span>}
          <span className="mission-card__scope">{task.workspaceName || task.scope || ""}</span>
          {canExpand && (
            <span className="mission-card__caret">
              {open ? <ChevronDown size={15} /> : <ChevronRight size={15} />}
            </span>
          )}
        </div>

        <div className="mission-card__title" title={purpose}>
          {purpose}
        </div>

        <div className="mission-card__detail">
          <div className="mission-card__field">
            <span className="mission-card__field-k">进度</span>
            <span className="mission-card__field-v">{progressText}</span>
          </div>
          <div className="mission-card__field">
            <span className="mission-card__field-k">下一步</span>
            <span className="mission-card__field-v">{nextStepText}</span>
          </div>
        </div>
      </div>

      {/* Pending approvals/asks — always visible when the task is blocked on you. */}
      {hasPending && (
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
        </div>
      )}

      {/* Section 4: time + hover-revealed actions. */}
      <div className="mission-card__foot">
        <span className="mission-card__time">
          {task.turnCount > 0 && <span>{task.turnCount} 轮 · </span>}
          {cost > 0 && <span>${cost.toFixed(3)} · </span>}
          {footTime}
        </span>
        <div className="mission-card__hover-actions">
          {canExpand && onOpenTab && (
            <button className="mission-card__open" onClick={() => onOpenTab(task.tabId)} title="打开此任务">
              <ExternalLink size={13} /> 打开
            </button>
          )}
          {!task.historical && (
            <div className="mission-card__menu">
              <button
                className="mission-card__more"
                title="更多操作"
                onClick={() => setMenuOpen((v) => !v)}
              >
                <MoreHorizontal size={16} />
              </button>
              {menuOpen && (
                <>
                  <div className="mission-card__menu-backdrop" onClick={() => setMenuOpen(false)} />
                  <div className="mission-card__menu-pop">
                    {archived ? (
                      <button
                        className="mission-card__menu-item"
                        onClick={() => {
                          void markOutcome("");
                          setMenuOpen(false);
                        }}
                        disabled={busy}
                      >
                        重新激活
                      </button>
                    ) : (
                      <>
                        <button
                          className="mission-card__menu-item"
                          onClick={() => {
                            void markOutcome("completed");
                            setMenuOpen(false);
                          }}
                          disabled={busy}
                        >
                          标记完成
                        </button>
                        <button
                          className="mission-card__menu-item mission-card__menu-item--danger"
                          onClick={() => {
                            void markOutcome("abandoned");
                            setMenuOpen(false);
                          }}
                          disabled={busy}
                        >
                          放弃任务
                        </button>
                      </>
                    )}
                  </div>
                </>
              )}
            </div>
          )}
        </div>
      </div>

      {open && (
        <div className="mission-card__body">
          {loadingSnap && <div className="mission-card__loading">正在生成摘要…</div>}
          {!loadingSnap && snap && <SnapshotView snap={snap} />}
          {!loadingSnap && !snap && <div className="mission-card__loading">暂无摘要</div>}
          <div className="mission-card__snap-meta">
            {snap && <span className="mission-card__snap-status">{snapStatus(snap, task)}</span>}
            <button
              className="mission-btn mission-btn--refresh-snap"
              onClick={refreshManual}
              disabled={loadingSnap || busy}
            >
              刷新摘要
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// snapStatus says whether the displayed summary is current.
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

// SnapshotView — expand-only detail: the dead-ends (what was tried and failed).
function SnapshotView({ snap }: { snap: TaskSnapshot }) {
  if (!snap.deadEnds || snap.deadEnds.length === 0) return null;
  return (
    <div className="mission-snap">
      <Section label="走不通的路" emphasis>
        <ul className="mission-snap__list mission-snap__list--deadends">
          {snap.deadEnds.map((d, i) => (
            <li key={i}>🚫 {d}</li>
          ))}
        </ul>
      </Section>
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
