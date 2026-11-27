import path from "node:path";

import { expect, test, type Page } from "@playwright/test";

// Captures pull-request evidence from the test-only contract fixture server. Every image is fixture data:
// no cluster, operator token or real installation is involved. The CI job dashboard-preview.yml runs this
// spec once per relevant pull request and uploads the PNGs; `PAGERLESS_SCREENSHOTS=1 npm run test:e2e --
// screenshots` reproduces them locally. Release documentation never references these files.
const enabled = process.env.PAGERLESS_SCREENSHOTS === "1";
const operatorToken = process.env.PAGERLESS_FIXTURE_TOKEN ?? "test-token";

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#operator-token").fill(operatorToken);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
}

async function reset(page: Page): Promise<void> {
  await page.request.post("/__control/reset", { data: {} });
}

function capture(page: Page, name: string): Promise<Buffer> {
  return page.screenshot({
    path: path.resolve(process.cwd(), `e2e/screenshots/${name}.png`),
    fullPage: false,
  });
}

test.describe("pull request screenshots", () => {
  test("captures the completed conversation and proposal in both palettes", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_SCREENSHOTS=1 to capture the PR images");
    await reset(page);
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
      await capture(page, `fixture-conversation-${viewport}-${theme}`);
      await page.evaluate(() => {
        document.querySelector('[data-testid="proposal"]')?.scrollIntoView({ block: "center" });
      });
      await page.waitForTimeout(200);
      await capture(page, `fixture-proposal-${viewport}-${theme}`);
    }
  });

  test("captures an active turn, its tool timeline and the Stop control", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_SCREENSHOTS=1 to capture the PR images");
    test.skip(testInfo.project.name.startsWith("mobile"), "desktop evidence for the active turn");
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-running");

    await expect(page.getByTestId("page-status")).toContainText("Running");
    await expect(page.getByTestId("activity")).toContainText("pods_logs");
    await expect(page.getByRole("button", { name: "Stop investigation" })).toBeVisible();
    await capture(page, "fixture-active-1280x900");
  });

  test("captures sanitized evidence opened from a citation", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_SCREENSHOTS=1 to capture the PR images");
    test.skip(testInfo.project.name.startsWith("mobile"), "desktop evidence for the evidence dialog");
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-checkout");
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    await page.getByRole("button", { name: "Open evidence for c4 at /spec/replicas" }).click();
    const dialog = page.getByRole("dialog", { name: "Tool evidence" });
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("Cited location: /spec/replicas");
    await capture(page, "fixture-evidence-1280x900");
  });

  test("captures the mobile history drawer", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_SCREENSHOTS=1 to capture the PR images");
    test.skip(!testInfo.project.name.startsWith("mobile"), "narrow-viewport history sheet");
    await reset(page);
    await signIn(page);

    await page.getByRole("button", { name: "Open investigation history" }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByRole("heading", { name: "History" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: /Why is checkout timing out/ })).toBeVisible();
    await capture(page, "fixture-history-390x844");
  });
});
