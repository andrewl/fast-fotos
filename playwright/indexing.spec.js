const { test, expect } = require("@playwright/test");

test.setTimeout(240000);

test("full re-index displays every fixture image thumbnail", async ({ page }) => {
  await page.goto("/maintenance");
  await page.getByRole("button", { name: "Reindex all" }).click();

  await expect(page.locator("#index-progress")).toContainText("photos indexed", {
    timeout: 240000,
  });

  await page.goto("/search");
  await expect(page.locator("#gallery img")).toHaveCount(4);
});
