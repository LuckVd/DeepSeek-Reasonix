import { useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown, ChevronRight, ExternalLink } from "lucide-react";
import { app } from "../lib/bridge";
import type { MissionTask, TaskSnapshot } from "../lib/types";

// MissionCard is one task row on the Mission Control board. The collapsed head
// answers "what is it doing / does it need me" at a glance; expanding pulls a
// TaskSnapshot (purpose / progress / actions / dead-ends / next step) so the
// user can re-orient on a task without tabbing into it. The dead-ends section is
// the point of the whole feature — "what did this agent already try that failed".
//
// TODO(i18n): labels are inline Chinese for the M1 prototype; migrate to t() once
// the locale keys are added to locales/{en,zh,zh-TW}.ts.

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
}: {
  task: MissionTask;
  onOpenTab?: (tabId: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [snap, setSnap] = useState<TaskSnapshot | null>(null);
  const [loading, setLoading] = useState(false);

  // Historical sessions have no live tab to expand or jump into.
  const canExpand = !task.historical;
  const waiting = task.runtimeState === "waiting";
  const cost = task.costUsd ?? 0;

  async function toggle() {
    if (!canExpand) return;
    const next = !open;
    setOpen(next);
    if (next && !snap && !loading) {
      setLoading(true);
      try {
        setSnap(await app.TaskSnapshot(task.tabId));
      } catch {
        /* leave snap null; the body shows a fallback */
      }
      setLoading(false);
    }
  }

  const title = task.title || task.goal || "未命名任务";

  return (
    <div
      className={[
        "mission-card",
        `mission-card--${task.runtimeState}`,
        waiting ? "mission-card--waiting" : "",
        task.historical ? "mission-card--historical" : "",
        task.detached ? "mission-card--detached" : "",
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

      {open && (
        <div className="mission-card__body">
          {loading && <div className="mission-card__loading">正在生成摘要…</div>}
          {!loading && snap && <SnapshotView snap={snap} />}
          {!loading && !snap && <div className="mission-card__loading">暂无摘要</div>}
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
