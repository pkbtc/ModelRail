# ModelRail TypeScript SDK

The SDK wraps ModelRail's HTTP API with typed task creation, status reads, pagination, retries, cancellation, and bounded-backoff waiting. This package is included in the repository; publish it to your npm organization before using `npm install @modelrail/sdk` from another project.

## Quickstart

Install the published package, then set `MODELRAIL_URL` and `MODELRAIL_API_KEY` as server-side environment variables:

```sh
npm install @modelrail/sdk
```

```ts
import { ModelRailApiError, ModelRailClient, TaskFailedError } from "@modelrail/sdk";

const client = new ModelRailClient({
  baseUrl: process.env.MODELRAIL_URL!,
  apiKey: process.env.MODELRAIL_API_KEY!,
});

try {
  const accepted = await client.createTask(
    {
      task: "Summarize the report in five bullet points",
      input: reportText,
      provider: "openrouter",
      model: "openrouter/free",
      maxTokens: 700,
      timeoutSeconds: 90,
      responseFormat: {
        type: "json_schema",
        json_schema: {
          name: "summary",
          strict: true,
          schema: {
            type: "object",
            properties: { bullets: { type: "array", items: { type: "string" } } },
            required: ["bullets"],
            additionalProperties: false,
          },
        },
      },
    },
    { idempotencyKey: `report-${reportId}` },
  );

  const task = await client.waitForTask(accepted.id, { timeoutMs: 120_000 });
  console.log(JSON.parse(task.result ?? "{}"));
} catch (error) {
  if (error instanceof TaskFailedError) {
    console.error("Model task failed:", error.task.error_message);
  } else if (error instanceof ModelRailApiError) {
    console.error(`ModelRail rejected the request (${error.status}):`, error.message);
  } else {
    console.error("Could not complete the ModelRail request:", error);
  }
}
```

Keep the ModelRail API key in your backend environment. Do not bundle it into browser JavaScript. Reuse the same idempotency key when retrying a submission after a network timeout.

## Webhooks

Instead of waiting and polling, provide an HTTPS callback and a secret of at least 16 characters:

```ts
await client.createTask({
  task: "Classify the support ticket",
  input: ticketText,
  webhook: { url: process.env.TASK_CALLBACK_URL!, secret: process.env.TASK_CALLBACK_SECRET! },
});
```

Set `WEBHOOK_ENCRYPTION_KEY` on the ModelRail server to 64 hexadecimal characters (32 random bytes). Callbacks contain `task.completed`, `task.failed`, or `task.cancelled` events. The `X-ModelRail-Signature` header is `t=<unix-seconds>,v1=<hex-hmac-sha256>`; verify HMAC-SHA256 over `<timestamp>.<raw request body>` and reject timestamps outside your replay window. Delivery IDs are in `X-ModelRail-Delivery`; repeated deliveries use the same ID.
