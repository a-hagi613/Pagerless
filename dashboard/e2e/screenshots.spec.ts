import path from "node:path";

import { expect, test, type Page } from "@playwright/test";

// Captures the four documentation screenshots from the real Portal. Run it through `make portal-screenshots`
// (or set PAGERLESS_PORTAL_SCREENSHOTS=1, with a harness already serving PAGERLESS_PORTAL_URL and
// PAGERLESS_BOOTSTRAP_TOKEN set). It is skipped in the ordinary suite so a normal run writes nothing.
const enabled = process.env.PAGERLESS_PORTAL_SCREENSHOTS === "1";
const token = process.env.PAGERLESS_BOOTSTRAP_TOKEN ?? "";

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  const heading = page.getByRole("heading", { name: "Sign in to Pagerless" });
  if (await heading.isVisible()) {
    await page.locator("#bootstrap-token").fill(token);
    await page.getByTestId("login-submit").click();
  }
  await expect(page.getByRole("heading", { name: "Cluster health" })).toBeVisible();
}

test.describe("documentation screenshots", () => {
  test("captures the cluster view in both palettes", async ({ page }, testInfo) => {
    test.skip(!enabled, "set PAGERLESS_PORTAL_SCREENSHOTS=1 to capture the documentation images");
    await signIn(page);

    const viewport = testInfo.project.name.includes("mobile") ? "390x844" : "1440x900";
    for (const theme of ["light", "dark"] as const) {
      await page.getByRole("radio", { name: `${theme === "light" ? "Light" : "Dark"} theme` }).check();
      await expect(page.locator("html")).toHaveClass(new RegExp(theme));
      // One frame for the final paint, then the capture.
      await page.waitForTimeout(250);
      await page.screenshot({
        path: path.resolve(process.cwd(), `../docs/assets/portal-${theme}-${viewport}.png`),
        fullPage: false,
      });
    }
  });
});
