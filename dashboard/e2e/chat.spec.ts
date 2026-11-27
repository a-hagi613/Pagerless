import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type TestInfo } from "@playwright/test";

// These flows run against e2e/fixture-server.mjs, the test-only implementation of
// docs/investigation-api.md. Every value on screen comes from an HTTP response or the ordered SSE stream;
// the browser talks to the same client code the embedded dashboard ships. When PAGERLESS_PORTAL_URL is set
// the same spec runs against a real pagerless-server.

const operatorToken = process.env.PAGERLESS_FIXTURE_TOKEN ?? "test-token";

async function reset(page: Page, options: Record<string, unknown> = {}): Promise<void> {
  const response = await page.request.post("/__control/reset", { data: options });
  expect(response.status()).toBe(200);
}

function watchConsole(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() !== "error") {
      return;
    }
    const text = message.text();
    // The failure scenarios deliberately trigger 5xx responses and one dropped stream; Chromium reports
    // those as network console lines, not as application errors.
    if (/Failed to load resource|net::ERR|EventSource/i.test(text)) {
      return;
    }
    errors.push(text);
  });
  page.on("pageerror", (error) => {
    errors.push(error.message);
  });
  return errors;
}

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#operator-token").fill(operatorToken);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
}

async function openHistory(page: Page, testInfo: TestInfo, title: string | RegExp): Promise<void> {
  const scope = await historyScope(page, testInfo);
  await scope.getByRole("link", { name: title }).first().click();
}

async function historyScope(page: Page, testInfo: TestInfo) {
  if (testInfo.project.name.startsWith("mobile")) {
    await page.getByRole("button", { name: "Open investigation history" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    return dialog;
  }
  return page.getByRole("region", { name: "Investigation history" }).first();
}

async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "the page must not scroll horizontally").toBeLessThanOrEqual(1);
}

function expectCleanConsole(errors: string[]): void {
  expect(errors, "the browser console must stay clean").toEqual([]);
}

test.describe("dashboard chat", () => {
  test("signs in with an HttpOnly cookie and keeps the token out of browser storage", async ({
    page,
  }, testInfo) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);

    const cookies = await page.context().cookies();
    const session = cookies.find((cookie) => cookie.name === "pagerless_session");
    expect(session?.httpOnly).toBe(true);
    expect(session?.sameSite).toBe("Strict");

    const stored = await page.evaluate(() => {
      const entries: string[] = [];
      for (let index = 0; index < window.localStorage.length; index += 1) {
        const key = window.localStorage.key(index);
        if (key !== null) {
          entries.push(`${key}=${window.localStorage.getItem(key) ?? ""}`);
        }
      }
      return entries;
    });
    expect(stored.join("\n")).not.toContain(operatorToken);
    expect(stored.every((entry) => entry.startsWith("pagerless.theme="))).toBe(true);

    const scope = await historyScope(page, testInfo);
    await expect(scope.getByRole("navigation", { name: "Past investigations" })).toBeVisible();
    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("reads every attempt in order, cites evidence, and shows the draft pull request", async ({
    page,
  }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await openHistory(page, test.info(), "Why is checkout timing out?");

    await expect(page.getByRole("heading", { name: "Why is checkout timing out?" })).toBeVisible();
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    const activity = page.getByTestId("activity");
    await expect(activity.getByRole("listitem")).toHaveCount(4);
    await expect(activity).toContainText("resources_get");
    await expect(activity).toContainText("denied");
    await expect(activity).toContainText("truncated");
    await expect(activity).toContainText("pods_logs");
    // A retry is a new attempt with a new call ID, not a replacement for the denied one.
    await expect(activity.getByText("c1", { exact: true })).toHaveCount(1);
    await expect(activity.getByText("c4", { exact: true })).toHaveCount(1);

    // The citation opens the sanitized evidence, including the cited pointer.
    await page.getByRole("button", { name: "Open evidence for c4 at /spec/replicas" }).click();
    const evidence = page.getByRole("dialog", { name: "Tool evidence" });
    await expect(evidence).toBeVisible();
    await expect(evidence).toContainText('"replicas": 2');
    await expect(evidence).toContainText("Cited location: /spec/replicas");
    await page.keyboard.press("Escape");
    await expect(evidence).toBeHidden();

    // The same evidence is reachable from the activity row by keyboard.
    await activity.getByRole("button", { name: "Read evidence for resources_get attempt c1" }).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("dialog", { name: "Tool evidence" })).toContainText("Secret reads are outside");
    await page.keyboard.press("Escape");

    const proposal = page.getByTestId("proposal");
    await expect(proposal.getByRole("heading", { name: "Proposed change" })).toBeVisible();
    await expect(proposal).toContainText("deploy/checkout/deployment.yaml");
    await expect(proposal).toContainText("replicas: 3");
    await expect(proposal.getByRole("link", { name: "Open draft pull request" })).toHaveAttribute(
      "href",
      "https://github.com/example/shop/pull/42",
    );

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("answers a question with no failing object and no source change", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-nofix");

    await expect(page.getByRole("heading", { name: "Which workloads have no memory limits?" })).toBeVisible();
    await expect(page.getByTestId("page-status")).toContainText("Complete");
    await expect(page.getByText("Every workload in the shop namespace sets a memory limit.")).toBeVisible();
    await expect(page.getByText("No source change was proposed.")).toBeVisible();
    await expect(page.getByTestId("proposal")).toHaveCount(0);

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("shows an interrupted turn after a restart", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-interrupted");

    await expect(page.getByTestId("page-status")).toContainText("Interrupted");
    await expect(page.getByText("This turn was interrupted by a restart.")).toBeVisible();
    await expect(page.getByTestId("activity")).toContainText("resources_get");
    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("reconnects mid-turn without losing or duplicating an attempt", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-reconnect");

    await expect(page.getByTestId("page-status")).toContainText("Complete", { timeout: 30_000 });
    const activity = page.getByTestId("activity");
    await expect(activity.getByRole("listitem")).toHaveCount(3);
    await expect(activity.getByText("c40", { exact: true })).toHaveCount(1);
    await expect(activity.getByText("c41", { exact: true })).toHaveCount(1);
    await expect(activity.getByText("c42", { exact: true })).toHaveCount(1);
    await expect(activity).toContainText("denied");
    await expect(page.getByText("Webhook deliveries retry because the CronJob")).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("stops the active turn and returns the composer", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-running");

    await expect(page.getByTestId("page-status")).toContainText("Running");
    await page.getByRole("button", { name: "Stop investigation" }).click();
    await expect(page.getByTestId("page-status")).toContainText("Stopped");
    await expect(page.getByRole("button", { name: "Stop investigation" })).toHaveCount(0);
    await expect(page.getByLabel("Follow-up message")).toBeEnabled();

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("detaching the browser leaves the turn active", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-running");
    await expect(page.getByTestId("page-status")).toContainText("Running");

    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
    await page.goto("/pages/p-running");
    await expect(page.getByTestId("page-status")).toContainText("Running");
    await expect(page.getByRole("button", { name: "Stop investigation" })).toBeVisible();

    expectCleanConsole(errors);
  });

  test("sends a follow-up turn from the same conversation", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-nofix");
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    await page.getByLabel("Follow-up message").fill("What about the payments namespace?");
    await page.getByRole("button", { name: "Send follow-up" }).click();

    await expect(page.getByText("Investigation answer for: What about the payments namespace?")).toBeVisible();
    await expect(page.getByTestId("page-status")).toContainText("Complete");
    await expect(page.getByTestId("activity")).toHaveCount(2);
    await expect(page.getByTestId("activity").last()).toContainText("resources_list");

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("starts an investigation with optional scope hints", async ({ page }) => {    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);

    await page.getByLabel("Your question").fill("Why is checkout returning 503?");
    await page.getByRole("button", { name: "Namespace or resource hint (optional)" }).click();
    await page.getByLabel("Namespace").fill("shop");
    await page.getByLabel("Resource kind").fill("Deployment");
    await page.getByLabel("Resource name").fill("checkout");
    await page.getByRole("button", { name: "Start investigation" }).click();

    await expect(page).toHaveURL(/\/pages\/p-created-1$/);
    await expect(page.getByRole("heading", { name: "Why is checkout returning 503?" })).toBeVisible();
    await expect(page.getByTestId("page-status")).toContainText("Complete");
    await expect(page.getByText("Investigation answer for: Why is checkout returning 503?")).toBeVisible();
    await expect(page.getByText("No source change was proposed.")).toBeVisible();
    await expect(page.getByText("namespace shop")).toBeVisible();
    await expect(page.getByText("Deployment/checkout")).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("keeps a failed create and retries it with the same draft", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page, { failCreate: 1 });
    await signIn(page);

    await page.getByLabel("Your question").fill("Why did the retry fail?");
    await page.getByRole("button", { name: "Start investigation" }).click();
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("The message was not sent");
    await expect(page.getByLabel("Your question")).toHaveValue("Why did the retry fail?");

    await alert.getByRole("button", { name: "Try again" }).click();
    await expect(page).toHaveURL(/\/pages\/p-created-1$/);
    await expect(page.getByText("Investigation answer for: Why did the retry fail?")).toBeVisible();

    expectCleanConsole(errors);
  });

  test("recovers an authenticated install that becomes unreachable", async ({ page }) => {
    await reset(page);
    await signIn(page);
    // The cookie is still valid; only the service is down. A reload must show availability, not sign-in.
    await reset(page, { probeDown: true });
    await page.reload();
    await expect(page.getByRole("heading", { name: "Pagerless is not reachable" })).toBeVisible();
    // The service comes back; the same page retries without a reload.
    await reset(page);
    await page.getByRole("button", { name: "Try again" }).click();
    await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
  });

  test("recovers a failed history refresh", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await reset(page, { failRefresh: 1 });
    await signIn(page);
    const scope = await historyScope(page, testInfo);

    await scope.getByRole("button", { name: "Refresh investigation history" }).click();
    await expect(scope.getByText("History could not be refreshed")).toBeVisible();
    await scope.getByRole("button", { name: "Retry" }).click();
    await expect(scope.getByText("History could not be refreshed")).toHaveCount(0);
    await expect(scope.getByRole("navigation", { name: "Past investigations" })).toBeVisible();
    expectCleanConsole(errors);
  });

  test("shows an empty history without inventing an investigation", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await reset(page, { empty: true });
    await signIn(page);
    const scope = await historyScope(page, testInfo);

    await expect(scope.getByText("No investigations yet")).toBeVisible();
    await expect(scope.getByText("Ask a question and it will appear here")).toBeVisible();
    await expect(scope.getByRole("navigation", { name: "Past investigations" })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });

  test("submits with Ctrl+Enter and announces the pending state", async ({ page }) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);

    const composer = page.getByLabel("Your question");
    await composer.focus();
    await composer.fill("Why is billing paused?");
    await page.keyboard.press("Control+Enter");
    await expect(page).toHaveURL(/\/pages\/p-created-1$/);
    await expect(page.getByText("Investigation answer for: Why is billing paused?")).toBeVisible();
    expectCleanConsole(errors);
  });

  test("persists the theme without a wrong first paint", async ({ page }, testInfo) => {
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);
    const setTheme = async (label: "Dark" | "Light") => {
      if (testInfo.project.name.startsWith("mobile")) {
        await page.getByRole("button", { name: "Open investigation history" }).click();
        const dialog = page.getByRole("dialog");
        await expect(dialog).toBeVisible();
        await dialog.getByRole("radio", { name: `${label} theme` }).check();
        await page.keyboard.press("Escape");
        await expect(page.getByRole("dialog")).toBeHidden();
        return;
      }
      await page.getByRole("radio", { name: `${label} theme` }).check();
    };

    await setTheme("Dark");
    await expect(page.locator("html")).toHaveClass(/dark/);
    expect(await page.evaluate(() => window.localStorage.getItem("pagerless.theme"))).toBe("dark");
    await page.reload();
    await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();
    await expect(page.locator("html")).toHaveClass(/dark/);

    await setTheme("Light");
    await expect(page.locator("html")).toHaveClass(/light/);
    expectCleanConsole(errors);
  });

  test("has no automatically detectable accessibility violations", async ({ page }) => {
    await reset(page);
    await signIn(page);
    await page.goto("/pages/p-checkout");
    await expect(page.getByTestId("page-status")).toContainText("Complete");

    const results = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
      .analyze();
    const summary = results.violations.map((violation) => ({
      id: violation.id,
      help: violation.help,
      nodes: violation.nodes.map((node) => node.target.join(" ")),
    }));
    expect(summary).toEqual([]);
  });
});

test.describe("narrow viewport", () => {
  test("opens history in a focus-trapped sheet and returns focus", async ({ page }, testInfo) => {
    test.skip(!testInfo.project.name.startsWith("mobile"), "the history sheet only applies to the narrow project");
    const errors = watchConsole(page);
    await reset(page);
    await signIn(page);

    const trigger = page.getByRole("button", { name: "Open investigation history" });
    await trigger.click();
    const sheet = page.getByRole("dialog");
    await expect(sheet.getByRole("heading", { name: "History" })).toBeVisible();
    await expect(sheet.getByRole("link", { name: /Why is checkout timing out/ })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toBeHidden();
    await expect(trigger).toBeFocused();

    await expectNoHorizontalOverflow(page);
    expectCleanConsole(errors);
  });
});
