import { expect, test, type Page } from "@playwright/test";

const MOCK_ORIGIN = "http://127.0.0.1:8092";

// Every test proves the same invariant: the Portal runs against the API without a console error, and
// nothing overflows horizontally, at both viewports.
function watchConsole(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() !== "error") {
      return;
    }
    const text = message.text();
    // The signed-out probe and a deliberately refused token are real 401s on the two auth endpoints;
    // Chromium logs every failed fetch as a console error. Those are the documented refusals, not a
    // Page/API failure, so they are excluded by URL. Every other console error still fails the test.
    const url = message.location().url;
    if (/401 \(Unauthorized\)/.test(text) && /\/api\/auth\/(session|login)$/.test(url)) {
      return;
    }
    errors.push(text);
  });
  page.on("pageerror", (cause) => {
    errors.push(cause.message);
  });
  return errors;
}

async function resetFixtures(page: Page): Promise<void> {
  const response = await page.request.post(`${MOCK_ORIGIN}/__test/reset`);
  expect(response.ok()).toBeTruthy();
}

async function signIn(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByTestId("login-token")).toBeVisible();
  await page.getByTestId("login-token").fill("test-bootstrap-token");
  await page.getByTestId("login-submit").click();
  await expect(page.getByRole("heading", { name: "Cluster health" })).toBeVisible();
}

async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(1);
}

async function card(page: Page, name: string) {
  return page.locator(`#page-${name}`);
}

test.describe("desktop 1440×900", () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test("signs in, filters the inbox from the matrix, and resets from the brand lockup", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);

    // A wrong token is refused inline, not by a dialog.
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
    await page.getByTestId("login-token").fill("wrong-token");
    await page.getByTestId("login-submit").click();
    await expect(page.getByRole("alert")).toContainText("not accepted");

    await page.getByTestId("login-token").fill("test-bootstrap-token");
    await page.getByTestId("login-submit").click();
    await expect(page.getByRole("heading", { name: "Cluster health" })).toBeVisible();
    await expect(page.getByText("6 of 6 Pages · whole cluster")).toBeVisible();

    // A namespace tile is the filter control.
    await page.getByRole("button", { name: "Filter the inbox by namespace shop" }).click();
    await expect(page.getByText("2 of 6 Pages · namespace shop")).toBeVisible();
    await expect(page.getByRole("button", { name: "Filter the inbox by namespace shop" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    // The brand lockup clears it and returns to the whole cluster.
    await page.getByRole("link", { name: /Pagerless cluster pages/ }).click();
    await expect(page.getByText("6 of 6 Pages · whole cluster")).toBeVisible();

    // A node row narrows to that node's Pages and the Node Page itself.
    await page.getByRole("button", { name: /ip-10-0-2-21/ }).click();
    await expect(page.getByText("3 of 6 Pages · node ip-10-0-2-21")).toBeVisible();

    // Severity narrows further; the brand lockup resets that too.
    await page.getByRole("button", { name: /^Critical/ }).click();
    await expect(page.getByText("1 of 6 Pages · node ip-10-0-2-21 · critical severity")).toBeVisible();
    await page.getByRole("link", { name: /Pagerless cluster pages/ }).click();
    await expect(page.getByText("6 of 6 Pages · whole cluster")).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("expands a card into root cause, a streaming trace, and the stored diff", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    const proposed = await card(page, "checkout-7d9f");
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await expect(proposed.getByText("Root cause")).toBeVisible();
    await expect(proposed.getByText("Cold-start p99 is 2.41s")).toBeVisible();
    await expect(proposed.getByRole("heading", { name: "Proposed change" })).toBeVisible();
    await expect(proposed.getByText("apps/shop/checkout/deployment.yaml", { exact: true })).toBeVisible();
    await expect(proposed.getByText("timeoutSeconds: 4")).toBeVisible();
    await expect(proposed.getByRole("button", { name: /sha256:1f4c9ab2/ })).toBeVisible();

    // A running investigation grows over Server-Sent Events and moves phase without a reload.
    const investigating = await card(page, "report-worker-4m1p");
    await investigating.getByRole("button", { name: "Watch trace" }).click();
    const trace = investigating.getByRole("region", { name: "Agent trace" });
    const steps = trace.locator("ol > li");
    const initial = await steps.count();
    await expect.poll(async () => steps.count()).toBeGreaterThan(initial);
    await expect(investigating.getByText("Proposed", { exact: true })).toBeVisible();
    await expect(investigating.getByRole("heading", { name: "Proposed change" })).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("approves the displayed hash and links the created draft PR", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    const proposed = await card(page, "checkout-7d9f");
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await expect(proposed.getByText("sha256:1f4c9ab2", { exact: false }).first()).toBeVisible();
    await proposed.getByRole("button", { name: "Approve & create PR" }).click();

    await expect(proposed.getByRole("link", { name: /PR #419/ }).first()).toBeVisible();
    await expect(proposed.getByText("PR open")).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("dismisses a Page and records the phase", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    const proposed = await card(page, "payments-api-2xkq");
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await proposed.getByLabel("Reason (optional, recorded on the Page)").fill("handled in the platform runbook");
    await proposed.getByRole("button", { name: "Dismiss" }).click();

    await expect(proposed.getByText("Rejected", { exact: true })).toBeVisible();

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("opens the CLI drawer with full parity and a This Page tab", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    await page.getByRole("button", { name: "Open the CLI equivalent drawer" }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer.getByText("CLI equivalent")).toBeVisible();
    await expect(drawer.getByText("11 Portal actions")).toBeVisible();
    await expect(drawer.getByText("11 CLI verbs")).toBeVisible();
    await drawer.getByRole("tab", { name: "Decisions" }).click();
    await expect(drawer.getByText("kubectl page diff <name>")).toBeVisible();
    await page.keyboard.press("Escape");

    const proposed = await card(page, "checkout-7d9f");
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await proposed.getByRole("button", { name: "Open the CLI equivalent for this Page" }).click();
    await expect(drawer.getByRole("tab", { name: "This Page" })).toBeVisible();
    await drawer.getByRole("tab", { name: "This Page" }).click();
    await expect(drawer.getByText("kubectl page diff checkout-7d9f")).toBeVisible();
    await expect(drawer.getByText("kubectl page approve checkout-7d9f --create-pr")).toBeVisible();
    await page.keyboard.press("Escape");

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("saves settings, then renders an external credential owner read-only", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    await page.getByRole("link", { name: "Settings" }).click();
    await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

    const baseUrl = page.getByTestId("settings-ai-base-url");
    await expect(baseUrl).toHaveValue("http://ollama.monitoring.svc:11434/v1");
    await baseUrl.fill("http://vllm.monitoring.svc:8000/v1");
    await page.getByTestId("settings-ai-save").click();
    await expect(page.getByText("Saved. The masked fingerprint")).toBeVisible();

    await page.getByTestId("settings-ai-test").click();
    await expect(page.getByText("ai answered in 214ms")).toBeVisible();

    // External ownership: every field is configured, read-only, and a write is refused inline.
    const external = await page.request.post(`${MOCK_ORIGIN}/__test/ownership`, {
      data: { managedBy: "external" },
    });
    expect(external.ok()).toBeTruthy();
    await page.reload();

    await expect(page.getByTestId("settings-external-banner")).toBeVisible();
    await expect(page.getByText("Managed by an external secret manager.")).toBeVisible();
    await expect(baseUrl).toHaveAttribute("readonly", "");
    await expect(page.getByTestId("settings-ai-save")).toBeDisabled();

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });

  test("switches Light, Dark, and System, persists the choice, and follows the OS", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);

    const root = page.locator("html");
    await expect(root).toHaveClass(/light/);

    await page.getByRole("radio", { name: "Dark theme" }).click();
    await expect(root).toHaveClass(/dark/);
    await expect(root).toHaveCSS("color-scheme", "dark");

    await page.reload();
    await expect(root).toHaveClass(/dark/);
    await expect(page.getByRole("radio", { name: "Dark theme" })).toBeChecked();

    await page.getByRole("radio", { name: "Light theme" }).click();
    await expect(root).toHaveClass(/light/);
    await expect(root).toHaveCSS("color-scheme", "light");

    await page.emulateMedia({ colorScheme: "dark" });
    await page.getByRole("radio", { name: "System theme" }).click();
    await expect(root).toHaveClass(/dark/);
    await page.emulateMedia({ colorScheme: "light" });
    await expect(root).toHaveClass(/light/);

    // A deep link can pin a palette for screenshots without changing the stored choice.
    await page.goto("/?theme=dark");
    await expect(root).toHaveClass(/dark/);
    const stored = await page.evaluate(() => window.localStorage.getItem("pagerless.theme"));
    expect(stored).toBe("system");

    await expectNoHorizontalOverflow(page);
    expect(errors).toEqual([]);
  });
});

test.describe("mobile 390×844", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("runs the whole flow without horizontal scrolling", async ({ page }) => {
    const errors = watchConsole(page);
    await resetFixtures(page);
    await signIn(page);
    await expectNoHorizontalOverflow(page);

    await page.getByRole("button", { name: "Filter the inbox by namespace shop" }).click();
    await expect(page.getByText("2 of 6 Pages · namespace shop")).toBeVisible();
    await expectNoHorizontalOverflow(page);

    const proposed = await card(page, "checkout-7d9f");
    await proposed.getByRole("button", { name: "Review diff" }).click();
    await expect(proposed.getByRole("heading", { name: "Proposed change" })).toBeVisible();
    await expectNoHorizontalOverflow(page);

    await proposed.getByRole("button", { name: "Approve & create PR" }).click();
    await expect(proposed.getByRole("link", { name: /PR #419/ }).first()).toBeVisible();
    await expectNoHorizontalOverflow(page);

    // Theme switching is reachable at 390 px and survives the same persistence rule.
    await page.getByRole("radio", { name: "Dark theme" }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    await page.reload();
    await expect(page.locator("html")).toHaveClass(/dark/);
    await expectNoHorizontalOverflow(page);

    // The settings route is a real destination on a phone too.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
    await expectNoHorizontalOverflow(page);

    expect(errors).toEqual([]);
  });
});
