import { expect, test } from "@playwright/test";

const apiKey = process.env.MODELRAIL_E2E_API_KEY;

test.beforeEach(async ({ page }) => {
  test.skip(!apiKey, "MODELRAIL_E2E_API_KEY must be provided by the test environment");
  await page.addInitScript((token) => {
    window.sessionStorage.setItem("modelrail.apiKey", token);
  }, apiKey!);
});

test("submits a task, displays the result, and reloads saved history", async ({ page }) => {
  const taskName = `browser smoke ${Date.now()}`;
  let idempotencyKey = "";
  page.on("request", (request) => {
    if (request.method() === "POST" && new URL(request.url()).pathname === "/v1/tasks") {
      idempotencyKey = request.headers()["idempotency-key"] ?? "";
    }
  });
  await page.goto("/");
  await expect(page.getByText("API ONLINE")).toBeVisible();

  await page.getByLabel("Task name").fill(taskName);
  await page.getByLabel("Input payload").fill("Persist this browser smoke task across reloads.");
  await page.getByRole("button", { name: /Dispatch task/ }).click();
  await expect.poll(() => idempotencyKey).toMatch(/^browser-[0-9a-f-]{36}$/i);
  await expect(page.locator(".result-block")).toContainText("Mock provider received task", { timeout: 25_000 });

  await page.reload();
  await expect(page.getByRole("heading", { name: taskName })).toBeVisible();
  await expect(page.locator(".result-block")).toContainText("Mock provider received task");

  await page.getByLabel("Filter runs by status").selectOption("completed");
  await expect(page.getByRole("button", { name: new RegExp(taskName) })).toBeVisible();
});
