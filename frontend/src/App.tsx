import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, apiRequest, checkHealth, listTasks, type TaskAccepted, type TaskRecord, type TaskStatus } from "./api";
import { ApiKeyDialog } from "./components/ApiKeyDialog";
import { Icon } from "./components/Icon";
import { RunList } from "./components/RunList";
import { RunPanel, type RunSummary } from "./components/RunPanel";
import { Sidebar } from "./components/Sidebar";
import { TaskComposer, type TaskComposerSettings } from "./components/TaskComposer";

type HealthState = "checking" | "online" | "offline";

const apiKeyStorageKey = "modelrail.apiKey";

const initialTaskSettings: TaskComposerSettings = {
  provider: "",
  model: "",
  maxAttempts: "3",
  temperature: "",
  maxTokens: "",
  timeoutSeconds: "",
  responseFormat: "",
  jsonSchema: "{\n  \"type\": \"object\",\n  \"properties\": {},\n  \"required\": [],\n  \"additionalProperties\": false\n}",
  webhookURL: "",
  webhookSecret: "",
};

function readSessionKey() {
  try {
    return window.sessionStorage.getItem(apiKeyStorageKey) ?? "";
  } catch {
    return "";
  }
}

function mapTaskRecord(record: TaskRecord): RunSummary {
  return {
    id: record.id,
    task: record.task,
    provider: record.provider,
    model: record.model,
    status: record.status,
    attemptCount: record.attempt_count,
    maxAttempts: record.max_attempts,
    result: record.result,
    errorMessage: record.error_message,
    cancelRequested: record.cancel_requested,
    createdAt: record.created_at,
  };
}

function DashboardHeader({
  health,
  hasKey,
  onOpenKey,
}: {
  health: HealthState;
  hasKey: boolean;
  onOpenKey: () => void;
}) {
  const labels: Record<HealthState, string> = {
    checking: "CHECKING API",
    online: "API ONLINE",
    offline: "API OFFLINE",
  };

  return (
    <header className="topbar">
      <div className="breadcrumb"><span>WORKSPACE</span><b>/</b><strong>Overview</strong></div>
      <div className="topbar-actions">
        <div aria-live="polite" className={`system-status system-${health}`}><span className="system-status-dot" />{labels[health]}</div>
        <button className={`key-status-button ${hasKey ? "key-is-set" : ""}`} onClick={onOpenKey} type="button">
          <Icon name={hasKey ? "shield" : "key"} size={15} />
          <span>{hasKey ? "Key connected" : "Add API key"}</span>
          <Icon className="key-chevron" name="chevron-down" size={14} />
        </button>
        <button aria-label="More options" className="icon-button topbar-more"><Icon name="more" /></button>
      </div>
    </header>
  );
}

function MetricCard({ label, value, detail, icon, tone }: {
  label: string;
  value: string;
  detail: string;
  icon: "layers" | "activity" | "check";
  tone: string;
}) {
  return (
    <div className="metric-card">
      <div className={`metric-icon ${tone}`}><Icon name={icon} size={16} /></div>
      <span className="metric-label">{label}</span>
      <strong className="metric-value">{value}</strong>
      <small>{detail}</small>
    </div>
  );
}

function App() {
  const [apiKey, setApiKey] = useState(readSessionKey);
  const [keyDialogOpen, setKeyDialogOpen] = useState(false);
  const [health, setHealth] = useState<HealthState>("checking");
  const [activeProvider, setActiveProvider] = useState("unknown");
  const [task, setTask] = useState("");
  const [input, setInput] = useState("");
  const [taskSettings, setTaskSettings] = useState<TaskComposerSettings>(initialTaskSettings);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [authRejected, setAuthRejected] = useState(false);
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [statusFilter, setStatusFilter] = useState<TaskStatus | "all">("all");
  const [nextCursor, setNextCursor] = useState("");
  const [historyLoading, setHistoryLoading] = useState(false);
  const [runActionBusy, setRunActionBusy] = useState<{ id: string; action: "retry" | "cancel" | "delete" } | null>(null);
  const runsRef = useRef(runs);
  const pendingSubmissionRef = useRef<{ payload: string; key: string } | null>(null);
  runsRef.current = runs;

  useEffect(() => {
    let cancelled = false;
    let checking = false;
    const refresh = async () => {
      if (cancelled || checking || document.visibilityState !== "visible") return;
      checking = true;
      const healthResult = await checkHealth();
      if (!cancelled) {
        setHealth(healthResult.healthy ? "online" : "offline");
        setActiveProvider(healthResult.provider ?? "unknown");
      }
      checking = false;
    };
    const handleVisibilityChange = () => {
      if (document.visibilityState === "visible") void refresh();
    };

    void refresh();
    document.addEventListener("visibilitychange", handleVisibilityChange);
    return () => {
      cancelled = true;
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, []);

  useEffect(() => {
    if (!apiKey || authRejected) return;
    let cancelled = false;
    let timer = 0;

    const poll = async () => {
      const pending = runsRef.current.filter((run) => run.status === "queued" || run.status === "processing");
      if (pending.length > 0) {
        const updates = await Promise.allSettled(
          pending.map((run) => apiRequest<TaskRecord>(`/v1/tasks/${run.id}`, apiKey)),
        );
        if (!cancelled) {
          const records = new Map<string, RunSummary>();
          updates.forEach((update) => {
            if (update.status === "fulfilled") records.set(update.value.id, mapTaskRecord(update.value));
            if (update.status === "rejected" && update.reason instanceof ApiError && update.reason.status === 401) {
              setError("The API key was rejected. Update it to continue tracking runs.");
              setAuthRejected(true);
              setKeyDialogOpen(true);
            }
          });
          if (records.size > 0) {
            setRuns((current) => current.map((run) => records.get(run.id) ?? run));
          }
        }
      }

      if (!cancelled) timer = window.setTimeout(poll, 1_800);
    };

    void poll();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [apiKey, authRejected]);

  useEffect(() => {
    if (!apiKey || authRejected) {
      setRuns([]);
      setSelectedId(null);
      setNextCursor("");
      setHistoryLoading(false);
      return;
    }
    let cancelled = false;
    setHistoryLoading(true);
    void listTasks(apiKey, { status: statusFilter })
      .then((page) => {
        if (cancelled) return;
        const loadedRuns = page.items.map(mapTaskRecord);
        setRuns(loadedRuns);
        setNextCursor(page.next_cursor ?? "");
        setSelectedId((current) => loadedRuns.some((run) => run.id === current) ? current : loadedRuns[0]?.id ?? null);
      })
      .catch((loadError: unknown) => {
        if (cancelled) return;
        if (loadError instanceof ApiError && loadError.status === 401) {
          setAuthRejected(true);
          setKeyDialogOpen(true);
        }
        setError(loadError instanceof Error ? loadError.message : "Could not load task history.");
      })
      .finally(() => {
        if (!cancelled) setHistoryLoading(false);
      });
    return () => { cancelled = true; };
  }, [apiKey, authRejected, statusFilter]);

  function saveApiKey(key: string) {
    setApiKey(key);
    setAuthRejected(false);
    try {
      if (key) window.sessionStorage.setItem(apiKeyStorageKey, key);
      else window.sessionStorage.removeItem(apiKeyStorageKey);
    } catch {
      // The key still works for this render if browser storage is unavailable.
    }
    setKeyDialogOpen(false);
    setError("");
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (!apiKey) {
      setKeyDialogOpen(true);
      return;
    }

    setSubmitting(true);
    try {
      if (new TextEncoder().encode(input).byteLength > 1 << 20) {
        setError("This request is larger than the 1 MB API limit.");
        return;
      }
      let responseFormat: Record<string, unknown> | undefined;
      if (taskSettings.responseFormat === "json_object") {
        responseFormat = { type: "json_object" };
      } else if (taskSettings.responseFormat === "json_schema") {
        let schema: unknown;
        try {
          schema = JSON.parse(taskSettings.jsonSchema);
        } catch {
          setError("Enter a valid JSON schema.");
          return;
        }
        if (typeof schema !== "object" || schema === null || Array.isArray(schema)) {
          setError("The JSON schema must be an object.");
          return;
        }
        responseFormat = { type: "json_schema", json_schema: { name: "task_output", strict: true, schema } };
      }
      const webhookURL = taskSettings.webhookURL.trim();
      const webhookSecret = taskSettings.webhookSecret;
      if (Boolean(webhookURL) !== Boolean(webhookSecret)) {
        setError("Enter both a webhook URL and its signing secret, or leave both blank.");
        return;
      }
      const requestBody = {
        task: task.trim(),
        input,
        ...(taskSettings.provider ? { provider: taskSettings.provider } : {}),
        ...(taskSettings.model.trim() ? { model: taskSettings.model.trim() } : {}),
        ...(taskSettings.maxAttempts ? { max_attempts: Number(taskSettings.maxAttempts) } : {}),
        ...(taskSettings.temperature ? { temperature: Number(taskSettings.temperature) } : {}),
        ...(taskSettings.maxTokens ? { max_tokens: Number(taskSettings.maxTokens) } : {}),
        ...(taskSettings.timeoutSeconds ? { timeout_seconds: Number(taskSettings.timeoutSeconds) } : {}),
        ...(responseFormat ? { response_format: responseFormat } : {}),
        ...(webhookURL ? { webhook: { url: webhookURL, secret: webhookSecret } } : {}),
      };
      const payload = JSON.stringify(requestBody);
      if (!pendingSubmissionRef.current || pendingSubmissionRef.current.payload !== payload) {
        pendingSubmissionRef.current = { payload, key: `browser-${crypto.randomUUID()}` };
      }
      const accepted = await apiRequest<TaskAccepted>("/v1/tasks", apiKey, {
        method: "POST",
        headers: { "Idempotency-Key": pendingSubmissionRef.current.key },
        body: payload,
      });
      pendingSubmissionRef.current = null;
      const run: RunSummary = {
        id: accepted.id,
        task: task.trim(),
        provider: accepted.provider,
        model: accepted.model,
        status: accepted.status,
        attemptCount: 0,
        maxAttempts: 3,
        result: null,
        errorMessage: null,
        cancelRequested: false,
        createdAt: new Date().toISOString(),
      };
      setStatusFilter("all");
      setRuns((current) => [run, ...current.filter((item) => item.id !== run.id)]);
      setSelectedId(run.id);
      setInput("");
      setTaskSettings((current) => ({ ...current, webhookSecret: "" }));
    } catch (submitError) {
      if (submitError instanceof ApiError && submitError.status === 401) {
        setError("That API key was not accepted by this ModelRail server.");
        setAuthRejected(true);
        setKeyDialogOpen(true);
      } else if (submitError instanceof Error) {
        setError(submitError.message || "Could not dispatch this task.");
      } else {
        setError("Could not dispatch this task.");
      }
    } finally {
      setSubmitting(false);
    }
  }

  async function copyRunId(id: string) {
    try {
      await navigator.clipboard.writeText(id);
      setError("Run ID copied to clipboard.");
      window.setTimeout(() => setError((current) => current === "Run ID copied to clipboard." ? "" : current), 2_000);
    } catch {
      setError("Could not access the clipboard. Select the run ID to copy it.");
    }
  }

  async function loadMoreRuns() {
    if (!apiKey || !nextCursor || historyLoading) return;
    setHistoryLoading(true);
    try {
      const page = await listTasks(apiKey, { status: statusFilter, cursor: nextCursor });
      const loaded = page.items.map(mapTaskRecord);
      setRuns((current) => {
        const existing = new Set(current.map((run) => run.id));
        return [...current, ...loaded.filter((run) => !existing.has(run.id))];
      });
      setNextCursor(page.next_cursor ?? "");
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : "Could not load older runs.");
    } finally {
      setHistoryLoading(false);
    }
  }

  async function retryRun(id: string) {
    if (!apiKey) return;
    setRunActionBusy({ id, action: "retry" });
    try {
      const record = await apiRequest<TaskRecord>(`/v1/tasks/${id}/retry`, apiKey, { method: "POST" });
      const run = mapTaskRecord(record);
      setStatusFilter("all");
      setRuns((current) => [run, ...current.filter((item) => item.id !== run.id)]);
      setSelectedId(run.id);
    } catch (actionError) {
      setError(actionError instanceof Error ? actionError.message : "Could not retry this run.");
    } finally {
      setRunActionBusy(null);
    }
  }

  async function cancelRun(id: string) {
    if (!apiKey) return;
    setRunActionBusy({ id, action: "cancel" });
    try {
      const record = await apiRequest<TaskRecord>(`/v1/tasks/${id}/cancel`, apiKey, { method: "POST" });
      const run = mapTaskRecord(record);
      setRuns((current) => current.flatMap((item) => {
        if (item.id !== run.id) return [item];
        return statusFilter === "all" || statusFilter === run.status ? [run] : [];
      }));
      if (statusFilter !== "all" && statusFilter !== run.status && selectedId === run.id) setSelectedId(null);
    } catch (actionError) {
      setError(actionError instanceof Error ? actionError.message : "Could not cancel this run.");
    } finally {
      setRunActionBusy(null);
    }
  }

  async function deleteRun(id: string) {
    if (!apiKey) return;
    setRunActionBusy({ id, action: "delete" });
    try {
      await apiRequest<void>(`/v1/tasks/${id}`, apiKey, { method: "DELETE" });
      const remaining = runs.filter((run) => run.id !== id);
      setRuns(remaining);
      if (selectedId === id) setSelectedId(remaining[0]?.id ?? null);
    } catch (actionError) {
      setError(actionError instanceof Error ? actionError.message : "Could not delete this run.");
    } finally {
      setRunActionBusy(null);
    }
  }

  const selectedRun = runs.find((run) => run.id === selectedId) ?? runs[0] ?? null;
  const activeCount = runs.filter((run) => run.status === "queued" || run.status === "processing").length;
  const completedCount = runs.filter((run) => run.status === "completed").length;
  const sessionCount = runs.length;
  const providerLabel = activeProvider === "unknown"
    ? "PROVIDER UNKNOWN"
    : activeProvider === "mock"
      ? "MOCK PROVIDER"
      : activeProvider.toUpperCase();

  function selectRun(id: string) {
    setSelectedId(id);
    document.getElementById("overview")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  function updateTaskSetting(key: keyof TaskComposerSettings, value: string) {
    setTaskSettings((current) => ({ ...current, [key]: value }));
  }

  return (
    <div className="app-shell" id="overview">
      <Sidebar hasKey={Boolean(apiKey) && !authRejected} onOpenKey={() => setKeyDialogOpen(true)} />
      <main className="main-content">
        <DashboardHeader health={health} hasKey={Boolean(apiKey) && !authRejected} onOpenKey={() => setKeyDialogOpen(true)} />
        <div className="dashboard-content">
          <section className="welcome-row">
            <div className="welcome-copy">
              <div className="welcome-kicker"><span className="kicker-mark">MR</span> TASK ORCHESTRATION <span className="kicker-slash">/</span> PRIVATE INSTANCE</div>
              <h1>Make every task<br /><span>move with intent.</span></h1>
              <p>A quiet control room for your model workflows. Dispatch work, follow the queue, and keep the outcome in view.</p>
            </div>
            <div className="welcome-art" aria-hidden="true">
              <div className="art-grid" />
              <div className="art-ring art-ring-one" />
              <div className="art-ring art-ring-two" />
              <div className="art-core"><span>MR</span><i /></div>
              <div className="art-node art-node-a"><span />01 / INPUT</div>
              <div className="art-node art-node-b"><span />02 / QUEUE</div>
              <div className="art-node art-node-c"><span />03 / OUTPUT</div>
              <div className="art-connector connector-a" />
              <div className="art-connector connector-b" />
            </div>
          </section>

          <section aria-label="Session metrics" className="metrics-row">
            <MetricCard detail="in loaded history" icon="layers" label="Dispatched" tone="metric-lime" value={String(sessionCount).padStart(2, "0")} />
            <MetricCard detail="awaiting a final state" icon="activity" label="In motion" tone="metric-cyan" value={String(activeCount).padStart(2, "0")} />
            <MetricCard detail="in loaded history" icon="check" label="Completed" tone="metric-violet" value={String(completedCount).padStart(2, "0")} />
            <div className="worker-card"><span className="worker-card-label">WORKER POOL</span><div className="worker-avatars"><span>01</span><span>02</span><b><i /></b></div><strong>2 <small>workers ready</small></strong><span className="worker-pulse"><i /> Queue polling every second</span></div>
          </section>

          <div className="workspace-grid">
            <div className="primary-column">
              <TaskComposer
                hasKey={Boolean(apiKey)}
                input={input}
                onInputChange={setInput}
                onNeedKey={() => setKeyDialogOpen(true)}
                onSubmit={handleSubmit}
                onSettingsChange={updateTaskSetting}
                onTaskChange={setTask}
                providerName={providerLabel}
                settings={taskSettings}
                submitting={submitting}
                task={task}
              />
              {error && <div aria-live="polite" className={`feedback-message ${error === "Run ID copied to clipboard." ? "feedback-success" : "feedback-error"}`}><span>{error}</span><button aria-label="Dismiss message" className="icon-button" onClick={() => setError("")} type="button"><Icon name="x" size={15} /></button></div>}
              <RunList
                hasMore={Boolean(nextCursor)}
                loading={historyLoading}
                onLoadMore={() => void loadMoreRuns()}
                onSelect={selectRun}
                onStatusFilterChange={setStatusFilter}
                runs={runs}
                selectedId={selectedRun?.id ?? null}
                statusFilter={statusFilter}
              />
            </div>
            <div className="secondary-column">
              <RunPanel
                busyAction={runActionBusy}
                onCancel={(id) => void cancelRun(id)}
                onCopyId={(id) => void copyRunId(id)}
                onDelete={(id) => void deleteRun(id)}
                onRetry={(id) => void retryRun(id)}
                run={selectedRun}
              />
              <section className="panel pipeline-panel">
                <div className="pipeline-topline"><span className="eyebrow">HOW IT MOVES</span><span className="pipeline-live"><i /> LIVE</span></div>
                <div className="pipeline-flow">
                  <div className="pipeline-node"><span className="pipeline-node-icon"><Icon name="terminal" size={16} /></span><span>Request</span><small>HTTP / JSON</small></div>
                  <span className="pipeline-arrow"><i /></span>
                  <div className="pipeline-node"><span className="pipeline-node-icon queue-icon"><Icon name="layers" size={16} /></span><span>Queue</span><small>PostgreSQL</small></div>
                  <span className="pipeline-arrow"><i /></span>
                  <div className="pipeline-node"><span className="pipeline-node-icon worker-icon"><Icon name="cpu" size={16} /></span><span>Worker</span><small>{providerLabel.toLowerCase()}</small></div>
                </div>
                <div className="pipeline-foot"><span><span className="pipeline-foot-dot" /> 2 consumers active</span><span>Polling · 1s</span></div>
              </section>
              <div className="note-card"><div className="note-icon"><Icon name="circle-help" size={17} /></div><p><strong>Provider: {providerLabel.toLowerCase()}.</strong> {activeProvider === "mock" ? "Set OPENROUTER_API_KEY on the server to send tasks to OpenRouter." : activeProvider === "openrouter" ? "Tasks are being sent to OpenRouter using the server-configured model." : "The active provider will appear here when the server is reachable."}</p><a href="#new-task" aria-label="Go to task composer"><Icon name="arrow-up-right" size={15} /></a></div>
            </div>
          </div>

          <footer className="page-footer"><span>MODELRAIL <b>·</b> OPERATIONS CONSOLE</span><span>LOCAL TIME <b>{new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(new Date())}</b></span></footer>
        </div>
      </main>
      {keyDialogOpen && <ApiKeyDialog initialValue={apiKey} onClose={() => setKeyDialogOpen(false)} onSave={saveApiKey} />}
    </div>
  );
}

export default App;
