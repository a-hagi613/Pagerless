// The Portal's only network boundary. Every path here is served by pagerless-server; the built
// dashboard talks to the real endpoints and nothing else. A browser test replaces this server with an
// offline mock, never the other way round.

import type {
  ConnectorName,
  DecisionRequest,
  NewPageRequest,
  PageDetail,
  PagesResponse,
  Session,
  Settings,
  SettingsTestResult,
  Step,
  Phase,
  PullRequest,
  Decision,
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
  const session = await requestJSON<Session>("/api/auth/session", {}, signal);
  setCSRFToken(session.csrfToken);
  return session;
}

export async function login(token: string): Promise<{ expiresAt: string }> {
  return await requestJSON<{ expiresAt: string }>("/api/auth/login", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

export async function logout(): Promise<void> {
  await requestJSON<void>("/api/auth/logout", { method: "POST" });
  setCSRFToken("");
}

export async function getPages(signal?: AbortSignal): Promise<PagesResponse> {
  return await requestJSON<PagesResponse>("/api/pages", {}, signal);
}

export async function getPage(name: string, signal?: AbortSignal): Promise<PageDetail> {
  return await requestJSON<PageDetail>(`/api/pages/${encodeURIComponent(name)}`, {}, signal);
}

export async function createPage(body: NewPageRequest): Promise<PageDetail> {
  return await requestJSON<PageDetail>("/api/pages", { method: "POST", body: JSON.stringify(body) });
}

export async function decidePage(name: string, body: DecisionRequest): Promise<PageDetail> {
  return await requestJSON<PageDetail>(`/api/pages/${encodeURIComponent(name)}/decision`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export async function getSettings(signal?: AbortSignal): Promise<Settings> {
  return await requestJSON<Settings>("/api/settings", {}, signal);
}

export interface SettingsPatch {
  ai?: { baseUrl?: string; model?: string; apiKey?: string };
  repository?: { owner?: string; repo?: string; branch?: string; autoDraftPR?: boolean; token?: string };
  connectors?: { name: ConnectorName; enabled?: boolean; url?: string; token?: string }[];
}

export async function updateSettings(patch: SettingsPatch): Promise<Settings> {
  return await requestJSON<Settings>("/api/settings", { method: "PATCH", body: JSON.stringify(patch) });
}

export type TestTarget = "ai" | "github" | `connector:${ConnectorName}`;

export async function testSettings(target: TestTarget): Promise<SettingsTestResult> {
  return await requestJSON<SettingsTestResult>("/api/settings/test", {
    method: "POST",
    body: JSON.stringify({ target }),
  });
}

export interface TraceHandlers {
  onStep: (step: Step) => void;
  onPhase: (update: { phase: Phase; decision?: Decision; pullRequest?: PullRequest }) => void;
  onDone: () => void;
}

// The investigation trace arrives as Server-Sent Events. The stream is opened only for an expanded
// card and closed on collapse, so a long inbox never holds more than one connection per view.
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

  source.addEventListener("step", (event) => {
    const step = parse<Step>(event);
    if (step !== null) {
      handlers.onStep(step);
    }
  });
  source.addEventListener("phase", (event) => {
    const update = parse<{ phase: Phase; decision?: Decision; pullRequest?: PullRequest }>(event);
    if (update !== null) {
      handlers.onPhase(update);
    }
  });
  source.addEventListener("done", () => {
    handlers.onDone();
    close();
  });
  // A closed EventSource means the browser gave up (401, 404, or the server ended the response).
  // An open one is retrying; EventSource's own backoff is the reconnect policy.
  source.addEventListener("error", () => {
    if (source.readyState === EventSource.CLOSED) {
      handlers.onDone();
      close();
    }
  });

  return close;
}
