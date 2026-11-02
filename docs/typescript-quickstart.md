# TypeScript quickstart

This walkthrough submits one task to ModelRail, waits for completion, and handles API/provider errors.

## 1. Start ModelRail

Configure the server with a PostgreSQL URL, a bootstrap API key, and `OPENROUTER_API_KEY`. Start it with `go run .`; it listens on `http://localhost:8080` by default. The server applies database migrations during startup.

Create a scoped ModelRail API key with `tasks:read` and `tasks:write`. Keep this key on your application's server. From PowerShell, using the bootstrap admin key configured in `.env`:

```powershell
$headers = @{ Authorization = "Bearer $env:MODELRAIL_BOOTSTRAP_API_KEY" }
$body = @{ name = "quickstart app"; scopes = @("tasks:read", "tasks:write") } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/api-keys -Headers $headers -ContentType "application/json" -Body $body
```

Copy the one-time `token` from the response into your application server's `MODELRAIL_API_KEY` secret.

## 2. Install the SDK

After publishing `sdk/typescript` to your npm organization:

```sh
npm install @modelrail/sdk
```

For local development before publishing, build the package and install its folder:

```sh
cd sdk/typescript
npm install
npm run build
cd ../../your-typescript-app
npm install ../ModelRail/sdk/typescript
```

Set these in your application server environment:

```text
MODELRAIL_URL=http://localhost:8080
MODELRAIL_API_KEY=mr_your_scoped_key
```

## 3. Submit, wait, and handle errors

```ts
import {
  ModelRailApiError,
  ModelRailClient,
  TaskFailedError,
} from "@modelrail/sdk";

const modelrail = new ModelRailClient({
  baseUrl: process.env.MODELRAIL_URL!,
  apiKey: process.env.MODELRAIL_API_KEY!,
});

async function summarize(reportId: string, reportText: string) {
  try {
    const accepted = await modelrail.createTask(
      {
        task: "Summarize the report in five bullet points",
        input: reportText,
        provider: "openrouter",
        maxTokens: 700,
        timeoutSeconds: 90,
      },
      // Keep this stable if your app retries after a network timeout.
      { idempotencyKey: `report-${reportId}` },
    );

    const completed = await modelrail.waitForTask(accepted.id, { timeoutMs: 120_000 });
    return JSON.parse(completed.result ?? "null");
  } catch (error) {
    if (error instanceof TaskFailedError) {
      console.error("Model task failed:", error.task.error_message);
    } else if (error instanceof ModelRailApiError) {
      console.error(`ModelRail returned HTTP ${error.status}:`, error.message);
    } else {
      console.error("Could not complete the task:", error);
    }
    throw error;
  }
}
```

To require a structured response, add `responseFormat: { type: "json_object" }` or pass a `json_schema` object. See the full request shape in [API task options](api.md#task-options-and-completion-webhooks).

`waitForTask` polls with increasing intervals capped at five seconds. For applications that should not poll, add a webhook to the task request and process the signed completion event instead. Webhooks require `WEBHOOK_ENCRYPTION_KEY` on the ModelRail server; see [API task options and completion webhooks](api.md#task-options-and-completion-webhooks).

The SDK is intended for server-side TypeScript. Do not include a long-lived ModelRail API key in browser code.
