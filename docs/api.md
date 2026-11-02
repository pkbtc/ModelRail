# ModelRail API

Base URL is the ModelRail service origin. Send `Authorization: Bearer <token>` to `/v1/*` and `/metrics` routes. The service returns JSON errors as `{"error":"..."}`. Task payloads are limited by `TASK_MAX_INPUT_BYTES` (1 MiB by default); task names are limited to 120 bytes. Unknown JSON fields and trailing JSON values are rejected.

## Scopes

| Scope | Allows |
| --- | --- |
| `tasks:read` | Read task details and paginated history |
| `tasks:write` | Submit tasks |
| `tasks:manage` | Retry failed tasks and cancel active tasks |
| `tasks:delete` | Delete terminal tasks |
| `keys:manage` | Create, list, and revoke API keys within the key's workspace, restricted to scopes the caller can grant |
| `metrics:read` | Read workspace-level Prometheus metrics |
| `admin` | All scopes and scope grants |

The bootstrap key receives `admin`. Create regular keys with only the scopes they need. Key creation returns the raw token once; only its digest is stored. The last active admin key in a workspace cannot be revoked.

## Submit a task

`POST /v1/tasks` requires `tasks:write`. Optional `Idempotency-Key` values must be 1–128 ASCII letters, digits, `.`, `_`, `:`, or `-`. Replaying a key with the same normalized request returns the original run; using it with any different task option or callback configuration returns `409`.

```http
POST /v1/tasks HTTP/1.1
Authorization: Bearer mr_...
Content-Type: application/json
Idempotency-Key: support-ticket-901

{"task":"classify the support ticket","input":"I was charged twice this month."}
```

### Task options and completion webhooks

Task creation also accepts optional `provider` (`openrouter` or `mock`), `model`, `temperature` (0–2), `max_tokens` (1–32768), `max_attempts` (1–10), and `timeout_seconds`. A per-task timeout cannot exceed the server's `TASK_TIMEOUT`. OpenRouter tasks use the server's `OPENROUTER_API_KEY`; callers never send provider credentials. `response_format` accepts `{"type":"json_object"}` or a JSON Schema format:

```json
{
  "task": "Extract invoice fields",
  "input": "Invoice text...",
  "provider": "openrouter",
  "model": "openrouter/free",
  "max_tokens": 500,
  "timeout_seconds": 90,
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "invoice",
      "strict": true,
      "schema": {
        "type": "object",
        "properties": {"total": {"type": "number"}},
        "required": ["total"],
        "additionalProperties": false
      }
    }
  },
  "webhook": {
    "url": "https://app.example.com/modelrail/events",
    "secret": "a-random-secret-at-least-16-chars"
  }
}
```

Webhooks are optional. Configure `WEBHOOK_ENCRYPTION_KEY` as 64 hexadecimal characters (32 random bytes) on the ModelRail server before submitting a task with a webhook. Callback secrets are AES-GCM encrypted at rest. Callback URLs must use HTTPS port 443 and resolve to public IP addresses; private, loopback, and link-local destinations are blocked to prevent server-side request forgery. Redirects are not followed.

Terminal state changes enqueue `task.completed`, `task.failed`, or `task.cancelled` events. ModelRail retries network failures, HTTP 408/429, and HTTP 5xx responses up to ten times with exponential backoff; other non-2xx responses mark the delivery failed. The callback receives `X-ModelRail-Delivery`, `X-ModelRail-Event`, and `X-ModelRail-Signature` headers. The signature is `t=<unix-seconds>,v1=<hex>`, where `hex` is HMAC-SHA256 using the submitted secret over `<timestamp>.<raw request body>`. Use a short timestamp replay window and treat the delivery ID as an idempotency key because a timed-out callback may be delivered again.

Example verification in a Node.js webhook handler:

```ts
import { createHmac, timingSafeEqual } from "node:crypto";

function verifyModelRailWebhook(rawBody: Buffer, signature: string, secret: string) {
  const parts = Object.fromEntries(signature.split(",").map((part) => part.split("=", 2)));
  const timestamp = parts.t;
  const received = parts.v1;
  if (!timestamp || !received || Math.abs(Date.now() / 1000 - Number(timestamp)) > 300) return false;

  const expected = createHmac("sha256", secret).update(`${timestamp}.`).update(rawBody).digest();
  let supplied: Buffer;
  try {
    supplied = Buffer.from(received, "hex");
  } catch {
    return false;
  }
  return supplied.length === expected.length && timingSafeEqual(supplied, expected);
}
```

Read the request body as raw bytes before JSON parsing so the signature check uses the exact body ModelRail sent.

The TypeScript client and five-minute setup walkthrough are in [`sdk/typescript`](../sdk/typescript/README.md) and [docs/typescript-quickstart.md](typescript-quickstart.md).

New tasks return `202 Accepted`; idempotent replays return `200 OK`. Both include `id`, `provider`, `status`, and `status_url`.

## List and inspect runs

`GET /v1/tasks` requires `tasks:read`. It returns newest-first keyset pagination. `limit` defaults to 20 and must be between 1 and 100. `status` can be `queued`, `processing`, `completed`, `failed`, or `cancelled`. Pass `next_cursor` back as `cursor` to continue.

```http
GET /v1/tasks?status=failed&limit=20 HTTP/1.1
Authorization: Bearer mr_...
```

`GET /v1/tasks/{id}` returns one task in the caller's workspace. Input text is never included in list or detail responses. Results and sanitized failure summaries are available to workspace keys with `tasks:read`.

## Retry, cancel, and delete

- `POST /v1/tasks/{id}/retry` requires `tasks:manage` and creates a new queued task from a failed task. The new task's `parent_task_id` points to the original run. Non-failed tasks return `409`.
- `POST /v1/tasks/{id}/cancel` requires `tasks:manage`. Queued tasks become cancelled immediately. Processing tasks receive a cancellation request; the worker stops its provider request on the next heartbeat (10 seconds by default) and marks the run cancelled. Cancellation is best effort if an upstream provider has already completed the call.
- `DELETE /v1/tasks/{id}` requires `tasks:delete`. Only completed, failed, and cancelled tasks can be deleted; active tasks return `409`. Cancel active tasks first.

## API keys

`GET /v1/api-keys`, `POST /v1/api-keys`, and `DELETE /v1/api-keys/{id}` require `keys:manage`. API key bodies are scoped to the authenticated workspace. A non-admin key can only grant/revoke scopes it already has and cannot grant `admin`.

```json
{"name":"read-only dashboard","scopes":["tasks:read"]}
```

The create response includes `{ "key": { ...metadata }, "token": "mr_..." }`. Store the token securely; later list calls include only the prefix and metadata.

`POST /v1/workspaces` requires `admin` and creates an isolated workspace plus its first admin API key. The response includes the workspace, key metadata, and raw key token once. Workspace keys can only read and modify tasks within their own workspace.

## Health and metrics

- `GET /healthz` is a liveness check and does not depend on PostgreSQL.
- `GET /readyz` pings PostgreSQL and returns `503` until it is reachable.
- `GET /metrics` requires `metrics:read` and exposes Prometheus text format counters, task processing duration histogram, and queue status gauges for the caller's workspace.

Rate limits are per API key and per service process. A rejected request returns `429 Too Many Requests` and `Retry-After`. When running multiple HTTP replicas, apply a shared rate limit at the gateway.
