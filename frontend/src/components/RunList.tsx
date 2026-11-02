import type { TaskStatus } from "../api";
import type { RunSummary } from "./RunPanel";
import { Icon } from "./Icon";

type RunListProps = {
  runs: RunSummary[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  statusFilter: TaskStatus | "all";
  onStatusFilterChange: (status: TaskStatus | "all") => void;
  hasMore: boolean;
  loading: boolean;
  onLoadMore: () => void;
};

const statusLabels: Record<TaskStatus, string> = {
  queued: "Queued",
  processing: "Running",
  completed: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
};

function formatTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "Now"
    : new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" }).format(date);
}

export function RunList({ runs, selectedId, onSelect, statusFilter, onStatusFilterChange, hasMore, loading, onLoadMore }: RunListProps) {
  return (
    <section aria-labelledby="activity-title" className="panel activity-panel" id="activity">
      <div className="panel-heading activity-heading">
        <div>
          <p className="eyebrow">TASK HISTORY <span className="eyebrow-index">/ 03</span></p>
          <h2 id="activity-title">Recent runs</h2>
        </div>
        <label className="session-tag history-filter-label">
          <span className="sr-only">Filter runs by status</span>
          <select aria-label="Filter runs by status" onChange={(event) => onStatusFilterChange(event.target.value as TaskStatus | "all")} value={statusFilter}>
            <option value="all">ALL STATUSES</option>
            <option value="queued">QUEUED</option>
            <option value="processing">PROCESSING</option>
            <option value="completed">COMPLETED</option>
            <option value="failed">FAILED</option>
            <option value="cancelled">CANCELLED</option>
          </select>
        </label>
      </div>

      {runs.length === 0 ? (
        <div className="activity-empty"><Icon name="clock" size={18} /><span>{loading ? "Loading saved runs…" : "No runs match this filter yet."}</span></div>
      ) : (
        <div className="run-list">
          {runs.map((run) => (
            <button aria-current={run.id === selectedId ? "true" : undefined} className={`run-list-row ${run.id === selectedId ? "selected" : ""}`} key={run.id} onClick={() => onSelect(run.id)} type="button">
              <span className={`run-list-indicator indicator-${run.status}`}><Icon name={run.status === "completed" ? "check" : run.status === "failed" || run.status === "cancelled" ? "x" : run.status === "processing" ? "bolt" : "clock"} size={14} /></span>
              <span className="run-list-name"><strong>{run.task}</strong><small>{run.id.slice(0, 8)} · {run.provider}</small></span>
              <span className={`run-list-status status-text-${run.status}`}>{statusLabels[run.status]}</span>
              <span className="run-list-time">{formatTime(run.createdAt)}</span>
              <Icon className="row-arrow" name="arrow-up-right" size={14} />
            </button>
          ))}
        </div>
      )}
      <div className="activity-footnote"><span className="footnote-line" /> Saved in PostgreSQL · newest first</div>
      {hasMore && <button className="button button-quiet history-more" disabled={loading} onClick={onLoadMore} type="button">{loading ? "Loading…" : "Load older runs"}</button>}
    </section>
  );
}
