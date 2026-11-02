export type TaskStatus = "queued" | "processing" | "completed" | "failed" | "cancelled";

export type ResponseFormat =
  | { type: "json_object" }
  | {
      type: "json_schema";
      json_schema: {
        name: string;
        strict?: boolean;
        schema: Record<string, unknown>;
      };
    };

export type WebhookOptions = {
  url: string;
  /** Shared secret used to verify X-ModelRail-Signature. Keep it server-side. */
  secret: string;
};

export type CreateTaskRequest = {
  task: string;
  input: string;
  provider?: "openrouter" | "mock";
  model?: string;
  maxAttempts?: number;
  temperature?: number;
  maxTokens?: number;
  timeoutSeconds?: number;
  responseFormat?: ResponseFormat;
  webhook?: WebhookOptions;
};

export type CreateTaskOptions = {
  /** Reuse this key when retrying a submission after a network failure. */
  idempotencyKey?: string;
  signal?: AbortSignal;
};

export type TaskAccepted = {
  id: string;
  provider: string;
  model?: string;
  status: TaskStatus;
  status_url: string;
};

export type TaskRecord = {
  id: string;
  task: string;
  provider: string;
  model?: string;
  status: TaskStatus;
  attempt_count: number;
  max_attempts: number;
  temperature?: number;
  max_tokens?: number;
  timeout_seconds?: number;
  response_format?: ResponseFormat;
  webhook_url?: string;
  result: string | null;
  error_message: string | null;
  cancel_requested: boolean;
  parent_task_id: string | null;
  created_at: string;
  updated_at: string;
};

export type TaskPage = {
  items: TaskRecord[];
  next_cursor?: string;
};

export type ListTasksOptions = {
  status?: TaskStatus;
  limit?: number;
  cursor?: string;
  signal?: AbortSignal;
};

export type WaitOptions = {
  timeoutMs?: number;
  initialIntervalMs?: number;
  maxIntervalMs?: number;
  signal?: AbortSignal;
};

export type ModelRailClientOptions = {
  baseUrl: string;
  apiKey: string;
  fetch?: typeof fetch;
};

export class ModelRailApiError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ModelRailApiError";
    this.status = status;
  }
}

export class TaskFailedError extends Error {
  readonly task: TaskRecord;

  constructor(task: TaskRecord) {
    super(task.error_message || `ModelRail task ${task.id} failed`);
    this.name = "TaskFailedError";
    this.task = task;
  }
}

export class TaskCancelledError extends Error {
  readonly task: TaskRecord;

  constructor(task: TaskRecord) {
    super(`ModelRail task ${task.id} was cancelled`);
    this.name = "TaskCancelledError";
    this.task = task;
  }
}

export class ModelRailClient {
  private readonly baseUrl: string;
  private readonly apiKey: string;
  private readonly fetchImpl: typeof fetch;

  constructor(options: ModelRailClientOptions) {
    if (!options.baseUrl.trim()) throw new Error("ModelRail baseUrl is required");
    if (!options.apiKey.trim()) throw new Error("ModelRail apiKey is required");
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.apiKey = options.apiKey;
    this.fetchImpl = options.fetch ?? fetch;
  }

  async createTask(request: CreateTaskRequest, options: CreateTaskOptions = {}): Promise<TaskAccepted> {
    const body = {
      task: request.task,
      input: request.input,
      ...(request.provider ? { provider: request.provider } : {}),
      ...(request.model ? { model: request.model } : {}),
      ...(request.maxAttempts !== undefined ? { max_attempts: request.maxAttempts } : {}),
      ...(request.temperature !== undefined ? { temperature: request.temperature } : {}),
      ...(request.maxTokens !== undefined ? { max_tokens: request.maxTokens } : {}),
      ...(request.timeoutSeconds !== undefined ? { timeout_seconds: request.timeoutSeconds } : {}),
      ...(request.responseFormat ? { response_format: request.responseFormat } : {}),
      ...(request.webhook ? { webhook: request.webhook } : {}),
    };
    return this.request<TaskAccepted>("/v1/tasks", {
      method: "POST",
      headers: options.idempotencyKey ? { "Idempotency-Key": options.idempotencyKey } : undefined,
      body: JSON.stringify(body),
      signal: options.signal,
    });
  }

  getTask(id: string, signal?: AbortSignal): Promise<TaskRecord> {
    return this.request<TaskRecord>(`/v1/tasks/${encodeURIComponent(id)}`, { signal });
  }

  listTasks(options: ListTasksOptions = {}): Promise<TaskPage> {
    const query = new URLSearchParams();
    if (options.status) query.set("status", options.status);
    if (options.limit !== undefined) query.set("limit", String(options.limit));
    if (options.cursor) query.set("cursor", options.cursor);
    const suffix = query.size ? `?${query.toString()}` : "";
    return this.request<TaskPage>(`/v1/tasks${suffix}`, { signal: options.signal });
  }

  async waitForTask(id: string, options: WaitOptions = {}): Promise<TaskRecord> {
    const timeoutController = new AbortController();
    const timeout = options.timeoutMs === undefined
      ? undefined
      : setTimeout(() => timeoutController.abort(), Math.max(0, options.timeoutMs));
    const signal = options.signal
      ? AbortSignal.any([options.signal, timeoutController.signal])
      : timeoutController.signal;
    const deadline = options.timeoutMs === undefined ? Number.POSITIVE_INFINITY : Date.now() + options.timeoutMs;
    const maxInterval = Math.max(100, options.maxIntervalMs ?? 5000);
    let interval = Math.max(100, options.initialIntervalMs ?? 500);
    try {
      while (Date.now() < deadline) {
        if (signal.aborted) throw abortError();
        const task = await this.getTask(id, signal);
        if (task.status === "completed") return task;
        if (task.status === "failed") throw new TaskFailedError(task);
        if (task.status === "cancelled") throw new TaskCancelledError(task);
        const remaining = deadline - Date.now();
        if (remaining <= 0) break;
        await delay(Math.min(interval, remaining), signal);
        interval = Math.min(Math.ceil(interval * 1.6), maxInterval);
      }
    } catch (error) {
      if (timeoutController.signal.aborted && !options.signal?.aborted) {
        throw new Error(`Timed out while waiting for ModelRail task ${id}`);
      }
      throw error;
    } finally {
      if (timeout !== undefined) clearTimeout(timeout);
    }
    throw new Error(`Timed out while waiting for ModelRail task ${id}`);
  }

  retryTask(id: string, signal?: AbortSignal): Promise<TaskRecord> {
    return this.request<TaskRecord>(`/v1/tasks/${encodeURIComponent(id)}/retry`, { method: "POST", signal });
  }

  cancelTask(id: string, signal?: AbortSignal): Promise<TaskRecord> {
    return this.request<TaskRecord>(`/v1/tasks/${encodeURIComponent(id)}/cancel`, { method: "POST", signal });
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers);
    headers.set("Authorization", `Bearer ${this.apiKey}`);
    if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    let response: Response;
    try {
      response = await this.fetchImpl(`${this.baseUrl}${path}`, { ...init, headers });
    } catch (error) {
      if (init.signal?.aborted) throw abortError();
      throw new Error("Could not reach the ModelRail API", { cause: error });
    }
    const data: unknown = await response.json().catch(() => null);
    if (!response.ok) {
      const message = typeof data === "object" && data !== null && "error" in data && typeof data.error === "string"
        ? data.error
        : `ModelRail API returned HTTP ${response.status}`;
      throw new ModelRailApiError(message, response.status);
    }
    return data as T;
  }
}

function abortError(): Error {
  return new DOMException("The operation was aborted", "AbortError");
}

function delay(duration: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortError());
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, duration);
    const onAbort = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      reject(abortError());
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
