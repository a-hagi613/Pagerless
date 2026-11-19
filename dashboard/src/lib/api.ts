// The Portal's only network boundary. Every path here is served by pagerless-server; the built dashboard
// talks to the real endpoints and nothing else. The Playwright suite drives the same client against a real
// pagerless-server process, so nothing in this file may depend on a fixture.

import type {
  Decision,
  DecisionAnswer,
  PageDetail,
  PageFilters,
  PagesResponse,
  Phase,
  PullRequest,
  Session,
  Settings,
  SettingsTestResult,
  StartAnswer,
  Step,
  TargetOption,
  VersionInfo,
} from "./types";

interface ErrorBody {
  error?: { code?: string; message?: string } | string;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

export class UnauthenticatedError extends ApiError {
  constructor(message = "the session has ended; sign in again") {
    super(401, "unauthenticated", message);
    this.name = "UnauthenticatedError";
  }
}

// The session's CSRF token, repeated in X-CSRF-Token on every state-changing request.
let csrfToken = "";

export function setCSRFToken(token: string): void {
  csrfToken = token;
}

function errorMessage(body: ErrorBody, response: Response): { code: string; message: string } {
  const error = body.error;
  if (typeof error === "string") {
    return { code: "error", message: error };
  }
  if (error !== undefined && typeof error.message === "string") {
    return { code: error.code ?? "error", message: error.message };
  }
  return { code: "error", message: `${String(response.status)} ${response.statusText}` };
}

async function requestJSON<T>(path: string, init: RequestInit = {}, signal?: AbortSignal): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  if (method !== "GET" && method !== "HEAD" && csrfToken !== "") {
    headers.set("X-CSRF-Token", csrfToken);
  }
  const response = await fetch(path, { ...init, headers, signal, credentials: "same-origin" });
  if (response.status === 401) {
    throw new UnauthenticatedError();
  }
  if (!response.ok) {
    let body: ErrorBody = {};
    try {
      body = (await response.json()) as ErrorBody;
    } catch {
      // The body is not JSON; the status line is the message.
    }
    const { code, message } = errorMessage(body, response);
    throw new ApiError(response.status, code, message);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

export async function getSession(signal?: AbortSignal): Promise<Session> {
  const session = await requestJSON<Session>("/api/session", {}, signal);
  setCSRFToken(session.csrfToken);
  return session;
}

export async function login(token: string): Promise<{ expiresAt: string }> {
  const answer = await requestJSON<{ expiresAt: string }>("/api/auth/login", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
  // The cookie is set by the login response, so the CSRF token is only available from the session route.
  await getSession();
  return answer;
}

export async function logout(): Promise<void> {
  await requestJSON<void>("/api/auth/logout", { method: "POST" });
  setCSRFToken("");
}

export async function getPages(
  filters: PageFilters = {},
  signal?: AbortSignal,
): Promise<PagesResponse> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "") {
      query.set(key, String(value));
    }
  }
  const encoded = query.toString();
  return await requestJSON<PagesResponse>(encoded === "" ? "/api/pages" : `/api/pages?${encoded}`, {}, signal);
}

export async function getPage(name: string, signal?: AbortSignal): Promise<PageDetail> {
  return await requestJSON<PageDetail>(`/api/pages/${encodeURIComponent(name)}`, {}, signal);
}

export interface NewPageRequest {
  kind: string;
  namespace?: string;
  name: string;
  hint?: string;
}

export async function createPage(body: NewPageRequest): Promise<StartAnswer> {
  return await requestJSON<StartAnswer>("/api/pages", { method: "POST", body: JSON.stringify(body) });
}

export interface DecisionRequest {
  // The API accepts approve or dismiss, and records Approved or Rejected.
  decision: "approve" | "dismiss";
  proposalHash: string;
  reason?: string;
}

export async function decidePage(name: string, body: DecisionRequest): Promise<DecisionAnswer> {
  return await requestJSON<DecisionAnswer>(`/api/pages/${encodeURIComponent(name)}/decision`, {
    method: "POST",
    body: JSON.stringify({
      decision: body.decision,
      proposalHash: body.proposalHash,
      ...(body.reason === undefined || body.reason === "" ? {} : { reason: body.reason }),
    }),
  });
}

export async function getTargets(signal?: AbortSignal): Promise<TargetOption[]> {
  const answer = await requestJSON<{ targets: TargetOption[] }>("/api/targets", {}, signal);
  return answer.targets;
}

export async function getSettings(signal?: AbortSignal): Promise<Settings> {
  return await requestJSON<Settings>("/api/settings", {}, signal);
}

export interface SettingsPatch {
  ai?: { baseUrl?: string; model?: string; apiKey?: string };
  repository?: { owner?: string; repo?: string; token?: string; apiUrl?: string };
  connectors?: { name: string; token?: string }[];
}

export async function updateSettings(patch: SettingsPatch): Promise<Settings> {
  return await requestJSON<Settings>("/api/settings", { method: "PATCH", body: JSON.stringify(patch) });
}

export async function unsetSetting(key: string): Promise<Settings> {
  return await requestJSON<Settings>(`/api/settings/${encodeURIComponent(key)}`, { method: "DELETE" });
}

export type TestTarget = "ai" | "github" | `connector:${string}`;

export async function testSettings(target: TestTarget): Promise<SettingsTestResult> {
  return await requestJSON<SettingsTestResult>("/api/settings/test", {
    method: "POST",
    body: JSON.stringify({ target }),
  });
}

export async function getVersion(signal?: AbortSignal): Promise<VersionInfo> {
  return await requestJSON<VersionInfo>("/api/version", {}, signal);
}

export interface TraceHandlers {
  onSnapshot: (record: PageDetail) => void;
  onStep: (event: { step: Step; finding?: PageDetail["findings"][number]; stepCount: number }) => void;
  onPhase: (update: { phase: Phase; terminal: boolean; decision?: Decision; pullRequest?: PullRequest }) => void;
  onEnd: () => void;
}

// The investigation trace arrives as Server-Sent Events from /trace, the alias of /events. The stream is
// opened only for an expanded card and closed on collapse, so a long inbox never holds one connection per
// row. The first event is a snapshot, so a late subscriber still sees the steps it missed.
export function subscribeTrace(name: string, handlers: TraceHandlers): () => void {
  const source = new EventSource(`/api/pages/${encodeURIComponent(name)}/trace`, { withCredentials: true });
  let closed = false;

  const close = () => {
    if (!closed) {
      closed = true;
      source.close();
    }
  };

  // A malformed frame is dropped rather than thrown: one bad event must not tear down the trace or
  // surface as an uncaught browser error.
  const parse = <T,>(event: Event): T | null => {
    try {
      return JSON.parse((event as MessageEvent<string>).data) as T;
    } catch {
      return null;
    }
  };

  source.addEventListener("snapshot", (event) => {
    const record = parse<PageDetail>(event);
    if (record !== null) {
      handlers.onSnapshot(record);
    }
  });
  source.addEventListener("step", (event) => {
    const update = parse<{ step: Step; finding?: PageDetail["findings"][number]; stepCount: number }>(event);
    if (update !== null) {
      handlers.onStep(update);
    }
  });
  source.addEventListener("phase", (event) => {
    const update = parse<{ phase: Phase; terminal: boolean }>(event);
    if (update !== null) {
      handlers.onPhase(update);
    }
  });
  source.addEventListener("end", () => {
    handlers.onEnd();
    close();
  });
  // A closed EventSource means the browser gave up (401, 404, or the server ended the response).
  // An open one is retrying; EventSource's own backoff is the reconnect policy.
  source.addEventListener("error", () => {
    if (source.readyState === EventSource.CLOSED) {
      handlers.onEnd();
      close();
    }
  });

  return close;
}
