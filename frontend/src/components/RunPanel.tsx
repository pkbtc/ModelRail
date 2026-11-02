import type { TaskStatus } from "../api";
import { Icon } from "./Icon";

export type RunSummary = {
  id: string;
  task: string;
  provider: string;
  model?: string;
  status: TaskStatus;
  attemptCount: number;
  maxAttempts: number;
  result: string | null;
  errorMessage: string | null;
  cancelRequested: boolean;
  createdAt: string;
};

type RunPanelProps = {
  run: RunSummary | null;
  onCopyId: (id: string) => void;
  onRetry: (id: string) => void;
  onCancel: (id: string) => void;
  onDelete: (id: string) => void;
  busyAction: { id: string; action: "retry" | "cancel" | "delete" } | null;
};

const statusLabels: Record<TaskStatus, string> = {
  queued: "Queued",
  processing: "Processing",
  completed: "Completed",
  failed: "Failed",
  cancelled: "Cancelled",
};

function formatTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "Just now"
    : new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" }).format(date);
}

export function RunPanel({ run, onCopyId, onRetry, onCancel, onDelete, busyAction }: RunPanelProps) {
  return (
    <section aria-labelledby="run-panel-title" className="panel run-panel">
      <div className="panel-heading run-panel-heading">
        <div>
          <p className="eyebrow">RUN INSPECTOR <span className="eyebrow-index">/ 02</span></p>
          <h2 id="run-panel-title">Latest signal</h2>
        </div>
        {run && <span className={`status-pill status-${run.status}`}><span />{statusLabels[run.status]}</span>}
      </div>

      {run ? (
        <div className="run-detail">
          <div className="run-title-block">
            <div className="run-glyph"><Icon name="bolt" size={18} /></div>
            <div className="run-title-copy"><h3>{run.task}</h3><span>Submitted {formatTime(run.createdAt)}</span></div>
            <button aria-label="Copy run ID" className="icon-button copy-button" onClick={() => onCopyId(run.id)} title="Copy run ID" type="button"><Icon name="copy" size={15} /></button>
          </div>

          {(run.status === "queued" || run.status === "processing" || run.status === "failed") && (
            <div className="run-actions">
              {(run.status === "queued" || run.status === "processing") && (
                <button className="button button-quiet" disabled={busyAction !== null} onClick={() => onCancel(run.id)} type="button">
                  {busyAction?.id === run.id && busyAction.action === "cancel" ? "Cancelling…" : "Cancel run"}
                </button>
              )}
              {run.status === "failed" && (
                <button className="button button-primary" disabled={busyAction !== null} onClick={() => onRetry(run.id)} type="button">
                  {busyAction?.id === run.id && busyAction.action === "retry" ? "Retrying…" : "Retry run"}
                </button>
              )}
            </div>
          )}
          {(run.status === "completed" || run.status === "failed" || run.status === "cancelled") && (
            <div className="run-actions">
              <button className="button button-quiet" disabled={busyAction !== null} onClick={() => onDelete(run.id)} type="button">
                {busyAction?.id === run.id && busyAction.action === "delete" ? "Deleting…" : "Delete run"}
              </button>
            </div>
          )}

          <div className="run-meta-grid">
            <div><span>RUN ID</span><strong className="run-id-text">{run.id.slice(0, 8)}…{run.id.slice(-5)}</strong></div>
            <div><span>PROVIDER</span><strong><i className="provider-dot" /> {run.provider}</strong></div>
            {run.model && <div><span>MODEL</span><strong>{run.model}</strong></div>}
            <div><span>ATTEMPT</span><strong>{run.attemptCount || 0} <small>/ {run.maxAttempts || 3}</small></strong></div>
          </div>

          <div aria-label={`Task status: ${statusLabels[run.status]}`} className="run-timeline">
            <div className="timeline-step done">
              <span className="timeline-node"><Icon name="check" size={12} /></span>
              <span>Queued</span>
            </div>
            <span className={`timeline-line ${run.status === "processing" || run.status === "completed" ? "active" : ""}`} />
            <div className={`timeline-step ${run.status === "processing" ? "current" : run.status === "completed" ? "done" : run.status === "failed" || run.status === "cancelled" ? "error" : ""}`}>
              <span className="timeline-node">{run.status === "completed" ? <Icon name="check" size={12} /> : run.status === "failed" || run.status === "cancelled" ? <Icon name="x" size={12} /> : <span />}</span>
              <span>Worker</span>
            </div>
            <span className={`timeline-line ${run.status === "completed" ? "active" : ""}`} />
            <div className={`timeline-step ${run.status === "completed" ? "done" : ""}`}>
              <span className="timeline-node">{run.status === "completed" ? <Icon name="check" size={12} /> : <span />}</span>
              <span>Result</span>
            </div>
          </div>

          {run.status === "completed" && run.result && (
            <div className="result-block">
              <div className="result-heading"><span><Icon name="sparkles" size={14} /> OUTPUT</span><span className="result-mock-label">MODEL RESPONSE</span></div>
              <p>{run.result}</p>
            </div>
          )}
          {run.status === "failed" && (
            <div className="error-block"><strong>Run failed</strong><p>{run.errorMessage || "The worker could not complete this task."}</p></div>
          )}
          {run.status === "cancelled" && (
            <div className="run-waiting">This run was cancelled.</div>
          )}
          {(run.status === "queued" || run.status === "processing") && (
            <div aria-live="polite" className="run-waiting"><span className="pulse-dot" />{run.cancelRequested ? "Cancellation requested…" : run.status === "queued" ? "Waiting for an available worker…" : "Worker is processing this task…"}</div>
          )}
        </div>
      ) : (
        <div className="empty-inspector">
          <div className="empty-orbit"><span /><i /><b /></div>
          <strong>Ready when you are</strong>
          <p>Dispatch a task and its live journey will appear here.</p>
          <div className="empty-state-code"><span>await</span> modelrail.dispatch<span>()</span></div>
        </div>
      )}
    </section>
  );
}
