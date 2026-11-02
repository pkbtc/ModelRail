export type TaskStatus = "queued" | "processing" | "completed" | "failed" | "cancelled";

export type TaskRecord = {
  id: string;
  task: string;
  input?: string;
  provider: string;
  model?: string;
  status: TaskStatus;
  attempt_count: number;
  max_attempts: number;
  temperature?: number;
  max_tokens?: number;
  timeout_seconds?: number;
  response_format?: Record<string, unknown>;
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

export type TaskAccepted = {
  id: string;
  provider: string;
  model?: string;
  status: TaskStatus;
  status_url: string;
};

export async function listTasks(apiKey: string, options: { status?: TaskStatus | "all"; cursor?: string } = {}) {
  const query = new URLSearchParams({ limit: "20" });
  if (options.status && options.status !== "all") query.set("status", options.status);
  if (options.cursor) query.set("cursor", options.cursor);
  return apiRequest<TaskPage>(`/v1/tasks?${query.toString()}`, apiKey);
}

export class ApiError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export async function apiRequest<T>(
  path: string,
  apiKey: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${apiKey}`);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(path, { ...init, headers });
  const data: unknown = await response.json().catch(() => null);

  if (!response.ok) {
    const message =
      typeof data === "object" &&
      data !== null &&
      "error" in data &&
      typeof data.error === "string"
        ? data.error
        : `Request failed with status ${response.status}`;
    throw new ApiError(message, response.status);
  }

  return data as T;
}

export async function checkHealth(): Promise<{ healthy: boolean; provider: string | null }> {
  try {
    const response = await fetch("/healthz", { cache: "no-store" });
    if (!response.ok) return { healthy: false, provider: null };
    const data: unknown = await response.json().catch(() => null);
    const provider =
      typeof data === "object" &&
      data !== null &&
      "provider" in data &&
      typeof data.provider === "string"
        ? data.provider
        : null;
    return { healthy: true, provider };
  } catch {
    return { healthy: false, provider: null };
  }
}
