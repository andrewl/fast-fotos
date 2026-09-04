const { defineConfig, devices } = require("@playwright/test");

const baseURL = process.env.PLAYWRIGHT_BASE_URL || "http://127.0.0.1:18090";

module.exports = defineConfig({
  testDir: "./playwright",
  fullyParallel: true,
  reporter: "list",
  globalTeardown: "./playwright/global-teardown.js",
  use: {
    baseURL,
    trace: "on-first-retry",
  },
  webServer: {
    command: "scripts/playwright-server.sh",
    url: baseURL,
    reuseExistingServer: false,
    timeout: 240000,
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
