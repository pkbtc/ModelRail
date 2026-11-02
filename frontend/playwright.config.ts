import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: { timeout: 15_000 },
  reporter: "list",
  use: {
    baseURL: process.env.MODELRAIL_E2E_URL ?? "http://127.0.0.1:8080",
    browserName: "chromium",
    trace: "retain-on-failure",
  },
});
