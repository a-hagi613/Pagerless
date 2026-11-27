import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// Regression for the PR 133 acceptance report: on a 390x844 viewport a real investigation response
// (long JSON pointers and a large nested tool result) widened the page, which stretched the centered
// Tool evidence dialog and pushed its Close control outside the screen. The dialog stayed open on
// desktop because the layout viewport never grows there.
//
// The fixture is deterministic: p-large-evidence reuses the genuine sanitized evidence payload captured
// from the acceptance run (src/lib/__fixtures__/investigation.json), so no live server or token is needed.
const operatorToken = process.env.PAGERLESS_FIXTURE_TOKEN ?? "test-token";
const evidenceTrigger = "Open evidence for c-large at /items/0/spec/containers/0/envFrom";

async function reset(page: Page): Promise<void> {
  const response = await page.request.post("/__control/reset", { data: {} });
  expect(response.status()).toBe(200);
}

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#operator-token").fill(operatorToken);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
}

function evidenceDialog(page: Page) {
  return page.getByRole("dialog", { name: "Tool evidence" });
}

async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "the page must not scroll horizontally").toBeLessThanOrEqual(1);
}

test.describe("narrow viewport evidence dialog", () => {
  test("keeps a large live response bounded and closes by pointer, keyboard and touch", async ({
    page,
  }, testInfo) => {
    test.skip(!testInfo.project.name.startsWith("mobile"), "the 390x844 layout is the regression");
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-large-evidence");
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    // The long, unbroken JSON pointer is what pushed the real page past 390px. It must wrap instead.
    await expectNoHorizontalOverflow(page);

    // Touch opens the dialog, exactly as an operator would tap the citation on a phone.
    await page.getByRole("button", { name: evidenceTrigger }).tap();
    const dialog = evidenceDialog(page);
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("region", { name: "Sanitized result" })).toContainText(
      "inventory-setting",
    );

    // The open dialog is part of the audited surface: a long result must not push the close
    // control outside the layout or introduce a contrast/labelling violation.
    const audit = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
      .analyze();
    expect(audit.violations.map((violation) => violation.id)).toEqual([]);

    // Bounded width: the dialog must sit inside the physical viewport, not a widened layout viewport.
    const viewport = page.viewportSize();
    expect(viewport).not.toBeNull();
    const box = await dialog.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(viewport!.width);
    const dialogOverflow = await dialog.evaluate(
      (element) => element.scrollWidth - element.clientWidth,
    );
    expect(dialogOverflow, "the dialog must not scroll horizontally").toBeLessThanOrEqual(1);

    // Scrolling the evidence body must not move the dialog or hide its Close control.
    const close = dialog.getByRole("button", { name: "Close" });
    const closeBefore = await close.boundingBox();
    await dialog.getByRole("region", { name: "Sanitized result" }).getByText("inventory-setting").first().scrollIntoViewIfNeeded();
    await dialog.evaluate((element) => {
      const body = element.querySelector("[data-evidence-body]");
      if (body !== null) {
        body.scrollTop = body.scrollHeight;
      }
    });
    const closeAfter = await close.boundingBox();
    expect(closeAfter).toEqual(closeBefore);
    await expect(close).toBeInViewport();

    // Pointer.
    await close.click();
    await expect(dialog).toBeHidden();

    // Page scroll is preserved across open and close.
    const scrollBefore = await page.evaluate(() => {
      window.scrollTo(0, document.body.scrollHeight);
      return window.scrollY;
    });
    expect(scrollBefore).toBeGreaterThan(0);
    await page.getByRole("button", { name: evidenceTrigger }).click();
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);

    // Keyboard: the dialog moves focus to Close, and Enter dismisses it.
    await page.getByRole("button", { name: evidenceTrigger }).click();
    await expect(dialog).toBeVisible();
    await expect(close).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(dialog).toBeHidden();

    // Touch close.
    await page.getByRole("button", { name: evidenceTrigger }).tap();
    await expect(dialog).toBeVisible();
    await close.tap();
    await expect(dialog).toBeHidden();
    await expectNoHorizontalOverflow(page);
  });
});
