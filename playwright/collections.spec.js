const { test, expect } = require("@playwright/test");

test.setTimeout(240000);

test("collections page load but there are no collections", async ({ page }) => {
  await page.goto("/collections");
  await expect(page.toContainText("collections", {
    timeout: 240000,
  }));

});
