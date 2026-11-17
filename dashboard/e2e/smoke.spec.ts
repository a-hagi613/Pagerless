import { readFileSync } from "node:fs";

import { expect, test } from "@playwright/test";

// The credentials that dashboard/e2e/keycloak.py provisioned. The file is
// written with mode 0600 and never printed.
interface SmokeState {
  username: string;
  password: string;
}

function credentials(): SmokeState {
  const path = process.env.PAGERLESS_SMOKE_STATE;
  if (path === undefined) {
    throw new Error("PAGERLESS_SMOKE_STATE is not set; run dashboard/e2e/run.sh");
  }
  return JSON.parse(readFileSync(path, "utf8")) as SmokeState;
}

test("an approver signs in, watches live steps, and approves", async ({ page }) => {
  const user = credentials();

  // Unauthenticated, the dashboard redirects to its sign-in route, which
  // redirects to Keycloak. The browser holds only the session cookie afterwards.
  await page.goto("/");
  await page.locator("#username").fill(user.username);
  await page.locator("#password").fill(user.password);
  await page.locator("#kc-login").click();

  await expect(page.getByRole("link", { name: "Start Triage" })).toBeVisible();
  await expect(page.getByText(user.username, { exact: true })).toBeVisible();

  // Start a Triage for the checkout Deployment through the dashboard.
  await page.getByRole("link", { name: "Start Triage" }).click();
  await page.locator("#workload").click();
  await page.getByRole("option", { name: /sandbox\/checkout/ }).click();
  await page.getByRole("button", { name: "Start Triage" }).click();
  await expect(page).toHaveURL(/\/triages\/[^/]+$/);

  // The detail view receives the agent's steps over Server-Sent Events; the
  // count must grow while the page is open, not only in the first snapshot.
  await expect(page.getByTestId("step").first()).toBeVisible();
  const arrival = await page.getByTestId("step").count();
  await expect.poll(async () => page.getByTestId("step").count()).toBeGreaterThan(arrival);

  // The Proposal arrives after the steps; approving records the Keycloak user.
  await page.locator("#decision-reason").fill("smoke test approval");
  await page.getByRole("button", { name: "Approve", exact: true }).click();

  await expect(page.getByTestId("decision-approver")).toHaveText(user.username);
  await expect(page.getByText("Approved", { exact: true })).toBeVisible();
});
