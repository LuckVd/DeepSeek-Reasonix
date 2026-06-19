import { useCallback, useEffect, useState } from "react";
import { RefreshCw, X } from "lucide-react";
import { app, onEvent } from "../lib/bridge";
import type { MissionTask } from "../lib/types";
import { MissionCard } from "./MissionCard";

// MissionsPanel is the Mission Control board: a glanceable, real-time aggregate
// of every task the user is running across desktop tabs (plus historical ones),
// so they never lose the thread when juggling several concurrent agents. It
// self-manages its data — pulls MissionTasks on mount and re-pulls on turn
// boundaries, approvals, and usage events (which carry tabId, so every tab's
// state updates live regardless of which is active).
//
// TODO(i18n): inline Chinese for the M1 prototype; migrate to t() later.

type Filter = "all" | "active" | "waiting" | "historical";

const FILTER_LABEL: Record<Filter, string> = {
  all: "全部",
  active: "活跃",
  waiting: "待我处理",
  historical: "历史",
};

// Kinds that change a task's board-relevant state. Text/reasoning deltas are
// intentionally excluded — they fire mid-turn and would thrash the list.
const REFRESH_KINDS = new Set([
  "turn_started",
  "turn_done",
  "approval_request",
  "ask_request",
  "usage",
]);

export function MissionsPanel({
  onClose,
  onOpenTab,
  fullPage = false,
}: {
  onClose: () => void;
  onOpenTab?: (tabId: string) => void;
  // fullPage renders the board as a filled page (no modal backdrop) so it can be
  // embedded as the main content area instead of floated as an overlay.
  fullPage?: boolean;
}) {
  const [tasks, setTasks] = useState<MissionTask[]>([]);
  const [filter, setFilter] = useState<Filter>("all");
  const [loading, setLoading] = useState(true);
  const [spinning, setSpinning] = useState(false);

  const refresh = useCallback(async () => {
    try {
      setTasks(await app.MissionTasks());
    } catch {
      /* keep last good list */
    }
    setLoading(false);
    setSpinning(false);
  }, []);

  useEffect(() => {
    refresh();
    return onEvent((e) => {
      if (REFRESH_KINDS.has(e.kind)) refresh();
    });
  }, [refresh]);

  const waitingCount = tasks.filter((t) => t.runtimeState === "waiting").length;

  const filtered = tasks.filter((t) => {
    switch (filter) {
      case "active":
        return !t.historical;
      case "waiting":
        return t.runtimeState === "waiting";
      case "historical":
        return t.historical;
      default:
        return true;
    }
  });

  const content = (
    <>
      <div className="mission-panel__head">
        <div className="mission-panel__title">
          <h2>任务总览</h2>
          {waitingCount > 0 && <span className="mission-panel__badge">{waitingCount} 待处理</span>}
        </div>
        <div className="mission-panel__actions">
          <button
            className="mission-panel__refresh"
            onClick={() => {
              setSpinning(true);
              refresh();
            }}
            title="刷新"
          >
            <RefreshCw size={15} className={spinning ? "mission-panel__spin" : ""} />
          </button>
          <button className="mission-panel__close" onClick={onClose} title="关闭">
            <X size={17} />
          </button>
        </div>
      </div>

      <div className="mission-panel__filters">
        {(Object.keys(FILTER_LABEL) as Filter[]).map((f) => (
          <button
            key={f}
            className={`mission-panel__filter${filter === f ? " mission-panel__filter--active" : ""}`}
            onClick={() => setFilter(f)}
          >
            {FILTER_LABEL[f]}
            {f === "waiting" && waitingCount > 0 ? ` (${waitingCount})` : ""}
          </button>
        ))}
      </div>

      <div className="mission-panel__body">
        {loading ? (
          <div className="mission-panel__empty">加载中…</div>
        ) : filtered.length === 0 ? (
          <div className="mission-panel__empty">没有任务</div>
        ) : (
          filtered.map((t) => <MissionCard key={t.tabId} task={t} onOpenTab={onOpenTab} onRefresh={refresh} />)
        )}
      </div>
    </>
  );

  if (fullPage) {
    return <div className="mission-panel mission-panel--page">{content}</div>;
  }

  return (
    <div className="mission-overlay" onClick={onClose}>
      <div className="mission-panel" onClick={(e) => e.stopPropagation()}>
        {content}
      </div>
    </div>
  );
}
