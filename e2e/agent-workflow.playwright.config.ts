import "./env";
import { defineConfig } from "@playwright/test";

const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH;

export default defineConfig({
  testDir: ".",
  testMatch: "agent-workflow.spec.ts",
  timeout: 90_000,
  workers: 1,
  retries: 0,
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? process.env.FRONTEND_ORIGIN ?? "http://localhost:3000",
    browserName: "chromium",
    ...(executablePath ? { launchOptions: { executablePath } } : {}),
    headless: true,
  },
});
