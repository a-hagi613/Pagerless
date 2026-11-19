import { expect, test, type Page } from "@playwright/test";

// The suite drives the real Portal served by a real pagerless-server. The Go harness
// (internal/server/portal_e2e_test.go, `make portal-e2e`) starts envtest, the server, and its real
// bootstrap-token sign-in, then runs this file once per viewport. Nothing here is mocked: every value on
// screen came from /api/* or the Server-Sent Event stream.

const token = process.env.PAGERLESS_BOOTSTRAP_TOKEN ?? "";

// Each project acts on its own pair of Proposed Pages, because the two projects share one server.
const targets = {
  desktop: { approve: "checkout", dismiss: "cart" },
  mobile: { approve: "search", dismiss: "billing" },
};

function projectTargets(projectName: string): { approve: string; dismiss: string } {
  if (projectName.startsWith("mobile")) {
    return targets.mobile;
  }
  return targets.desktop;
}

// watchConsole fails a test on any browser error. A handled API refusal is not a console error.
function watchConsole(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      errors.push(message.text());
    }
  });
  page.on("pageerror", (error) => {
    errors.push(error.message);
  });
  return errors;
}

async function signIn(page: Page): Promise<void> {
  expect(token, "PAGERLESS_BOOTSTRAP_TOKEN is set by the Go harness").not.toBe("");
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#bootstrap-token").fill(token);
  await page.getByTestId("login-submit").click();
  await expect(page.getByRole("heading", { name: "Cluster health" })).toBeVisible();
}

function card(page: Page, target: string) {
  return page.locator("article").filter({ has: page.getByRole("heading", { name: target, exact: true }) });
}

async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "the page must not scroll horizontally").toBeLessThanOrEqual(1);
}

async function expectNoConsoleErrors(errors: string[]): Promise<void> {
  expect(errors, "the browser console must stay clean").toEqual([]);
}

test.describe("the Portal against the real server", () => {
  test("signs in with the bootstrap token and renders the live matrix", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await signIn(page);

    // Every tile is a namespace with Pages, and the node tile carries the Node Page.
    await expect(page.getByRole("button", { name: "Filter the inbox by namespace sandbox" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Filter the inbox by namespace shop" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Filter the inbox by node ip-10-0-1-17" })).toBeVisible();
    await expect(page.getByTestId("inbox-scope")).toContainText("whole cluster");

    await expectNoHorizontalOverflow(page);
    await expectNoConsoleErrors(errors);
    await testInfo.attach("cluster", { body: await page.screenshot(), contentType: "image/png" });
  });

  test("filters from a namespace tile and the brand link clears it", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    const mine = projectTargets(testInfo.project.name);
    await signIn(page);

    await page.getByRole("button", { name: "Filter the inbox by namespace shop" }).click();
    await expect(page.getByTestId("inbox-scope")).toContainText("namespace shop");
    await expect(card(page, mine.approve)).toHaveCount(0);
    await expect(card(page, "audit")).toBeVisible();

    await page.getByRole("link", { name: /Pagerless cluster pages/ }).click();
    await expect(page.getByTestId("inbox-scope")).toContainText("whole cluster");
    await expect(card(page, mine.approve)).toBeVisible();

    await expectNoConsoleErrors(errors);
  });

  test("expands a Page, streams the trace, and approves the Proposal", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    const mine = projectTargets(testInfo.project.name);
    await signIn(page);

    // The live Page's trace grows while the card is open: the stream appends steps, it does not poll a
    // snapshot into place.
    const live = card(page, "payments");
    await live.getByRole("button", { name: "Watch trace" }).click();
    await expect(live.getByTestId("trace")).toBeVisible();
    const traceHeading = live.getByRole("heading", { name: /Investigation trace/ });
    const before = await traceHeading.textContent();
    await expect(traceHeading).not.toHaveText(before ?? "", { timeout: 30_000 });

    // The Proposal diff is the stored one, and the Diagnosis explains it.
    const proposed = card(page, mine.approve);
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await expect(proposed.getByTestId("diagnosis")).toContainText("needs more replicas");
    await expect(proposed.getByRole("region", { name: "Proposed change" })).toContainText("replicas: 3");

    // Approving moves the Phase and refreshes the inbox without a reload.
    await proposed.getByRole("button", { name: /Approve/ }).click();
    await expect(proposed).toContainText(/Approved|PR open/, { timeout: 30_000 });

    await expectNoHorizontalOverflow(page);
    await expectNoConsoleErrors(errors);
    await testInfo.attach("approved", { body: await page.screenshot(), contentType: "image/png" });
  });

  test("dismisses a Page with a reason", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    const mine = projectTargets(testInfo.project.name);
    await signIn(page);

    const dismissable = card(page, mine.dismiss);
    await dismissable.getByRole("button", { name: "Review diff" }).click();
    await dismissable.getByLabel(/Reason \(required to dismiss/).fill("handled in another change");
    await dismissable.getByRole("button", { name: "Dismiss" }).click();
    await expect(dismissable).toContainText("Rejected", { timeout: 30_000 });

    await expectNoConsoleErrors(errors);
  });

  test("saves the AI endpoint in settings", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await signIn(page);
    await page.getByRole("link", { name: "Settings" }).click();
    await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

    const model = `e2e-model-${testInfo.project.name}`;
    await page.getByTestId("settings-ai-model").fill(model);
    await page.getByTestId("settings-ai-save").click();
    await expect(page.getByRole("status").filter({ hasText: "Saved." })).toBeVisible();

    await page.reload();
    await expect(page.getByTestId("settings-ai-model")).toHaveValue(model);

    await expectNoHorizontalOverflow(page);
    await expectNoConsoleErrors(errors);
  });

  test("persists Light, Dark, and System with no wrong first paint", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await signIn(page);

    await page.getByRole("radio", { name: "Dark theme" }).check();
    await expect(page.locator("html")).toHaveClass(/dark/);
    const saved = await page.evaluate(() => window.localStorage.getItem("pagerless.theme"));
    expect(saved).toBe("dark");

    await page.reload();
    await expect(page.getByRole("heading", { name: "Cluster health" })).toBeVisible();
    await expect(page.locator("html")).toHaveClass(/dark/);
    expect(await page.evaluate(() => document.documentElement.style.colorScheme)).toBe("dark");

    await page.getByRole("radio", { name: "Light theme" }).check();
    await expect(page.locator("html")).toHaveClass(/light/);
    expect(await page.evaluate(() => document.documentElement.style.colorScheme)).toBe("light");

    await page.getByRole("radio", { name: "System theme" }).check();
    await page.emulateMedia({ colorScheme: "dark" });
    await expect(page.locator("html")).toHaveClass(/dark/);
    await page.emulateMedia({ colorScheme: "light" });
    await expect(page.locator("html")).toHaveClass(/light/);

    // The document ships no hardcoded palette and resolves the stored one before React runs.
    const shell = await (await page.request.get("/")).text();
    expect(shell).not.toMatch(/<html[^>]*class="[^"]*dark/);
    expect(shell).toContain("pagerless.theme");

    await expectNoConsoleErrors(errors);
    await testInfo.attach("theme", { body: await page.screenshot(), contentType: "image/png" });
  });
});
