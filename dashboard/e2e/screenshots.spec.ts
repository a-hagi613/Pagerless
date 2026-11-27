import path from "node:path";

import { expect, test, type Page } from "@playwright/test";

// Captures the pull-request screenshots from the same contract fixture server. It is skipped in the
// ordinary suite so a normal run writes nothing. `PAGERLESS_SCREENSHOTS=1 npm run test:e2e -- screenshots`
// writes them under dashboard/e2e/screenshots for the PR body.
const enabled = process.env.PAGERLESS_SCREENSHOTS === "1";
const operatorToken = process.env.PAGERLESS_FIXTURE_TOKEN ?? "test-token";

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#operator-token").fill(operatorToken);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
}

test.describe("pull request screenshots", () => {
  test("captures the conversation in both palettes", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_SCREENSHOTS=1 to capture the PR images");
    await page.request.post("/__control/reset", { data: {} });
    await signIn(page);
    await page.goto("/pages/p-checkout");
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    const viewport = testInfo.project.name.startsWith("mobile") ? "390x844" : "1280x900";
    for (const theme of ["light", "dark"] as const) {
      if (testInfo.project.name.startsWith("mobile")) {
        await page.getByRole("button", { name: "Open investigation history" }).click();
        const dialog = page.getByRole("dialog");
        await expect(dialog).toBeVisible();
        await dialog.getByRole("radio", { name: `${theme === "light" ? "Light" : "Dark"} theme` }).check();
        await page.keyboard.press("Escape");
        await expect(page.getByRole("dialog")).toBeHidden();
      } else {
        await page.getByRole("radio", { name: `${theme === "light" ? "Light" : "Dark"} theme` }).check();
      }
      await expect(page.locator("html")).toHaveClass(new RegExp(theme));
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.waitForTimeout(200);
      await page.screenshot({
        path: path.resolve(process.cwd(), `e2e/screenshots/checkout-${viewport}-${theme}.png`),
        fullPage: false,
      });
      await page.evaluate(() => {
        document.querySelector('[data-testid="proposal"]')?.scrollIntoView({ block: "center" });
      });
      await page.waitForTimeout(200);
      await page.screenshot({
        path: path.resolve(process.cwd(), `e2e/screenshots/proposal-${viewport}-${theme}.png`),
        fullPage: false,
      });
    }
  });
});
