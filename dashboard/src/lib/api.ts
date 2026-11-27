// The dashboard's only network boundary. Every path is the documented /api/v1 contract; there is no
// fixture, demo mode or local success fallback in production. A refused request is surfaced to the page
// that asked, and a 401 marks the browser session as ended.

import type {
  DiffResponse,
  EventsResponse,
  InvestigationEvent,
  InvestigationRequest,
  PageDetail,
  PagesResponse,
  StopAnswer,
  ToolEvidence,
  TurnAccepted,
} from "./contract";

interface ErrorBody {
  error?: { code?: string; message?: string } | string;
}

const base = "/api/v1";

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
  constructor(message = "the session has ended") {
    super(401, "unauthenticated", message);
    this.name = "UnauthenticatedError";
  }
}

let unauthorizedHandler: (() => void) | null = null;

// The auth provider registers here so any refused call can return the browser to sign-in. A listener is
// only notified; the token and cookie stay out of JavaScript.
export function setUnauthorizedHandler(handler: (() => void) | null): void {
  unauthorizedHandler = handler;
}

function errorDetails(body: ErrorBody, response: Response): { code: string; message: string } {
  const error = body.error;
  if (typeof error === "string") {
    return { code: "error", message: error };
  }
  if (error !== undefined && typeof error.message === "string") {
    return { code: error.code ?? "error", message: error.message };
  }
  return { code: "error", message: `${String(response.status)} ${response.statusText}`.trim() };
}

async function requestJSON<T>(
  path: string,
  init: RequestInit = {},
  signal?: AbortSignal,
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(`${base}${path}`, {
    ...init,
    headers,
    signal,
    credentials: "same-origin",
  });
  if (response.status === 401) {
    unauthorizedHandler?.();
    throw new UnauthenticatedError();
  }
  if (!response.ok) {
    let body: ErrorBody = {};
    try {
      body = (await response.json()) as ErrorBody;
    } catch {
      // A non-JSON refusal still has a status line worth showing.
    }
    const { code, message } = errorDetails(body, response);
    throw new ApiError(response.status, code, message);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

// exchangeSession trades the installer's operator token for an HttpOnly, SameSite=Strict cookie. The token
// exists only in the submitted request body: it is never stored, logged or put in a URL.
export async function exchangeSession(token: string): Promise<void> {
  await requestJSON<void>("/session", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

export async function listPages(signal?: AbortSignal): Promise<PagesResponse> {
  return await requestJSON<PagesResponse>("/pages", {}, signal);
}

export async function getPage(pageId: string, signal?: AbortSignal): Promise<PageDetail> {
  return await requestJSON<PageDetail>(`/pages/${encodeURIComponent(pageId)}`, {}, signal);
}

export async function createPage(
  body: InvestigationRequest,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<TurnAccepted> {
  return await requestJSON<TurnAccepted>(
    "/pages",
    {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: JSON.stringify(body),
    },
    signal,
  );
}

export async function replyToPage(
  pageId: string,
  body: InvestigationRequest,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<TurnAccepted> {
  return await requestJSON<TurnAccepted>(
    `/pages/${encodeURIComponent(pageId)}/turns`,
    {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: JSON.stringify(body),
    },
    signal,
  );
}

export async function stopTurn(pageId: string, signal?: AbortSignal): Promise<StopAnswer> {
  return await requestJSON<StopAnswer>(
    `/pages/${encodeURIComponent(pageId)}/stop`,
    { method: "POST" },
    signal,
  );
}

export async function getEvents(
  pageId: string,
  after: number,
  signal?: AbortSignal,
): Promise<InvestigationEvent[]> {
  const answer = await requestJSON<EventsResponse>(
    `/pages/${encodeURIComponent(pageId)}/events?after=${String(after)}`,
    {},
    signal,
  );
  return answer.events;
}

export async function getEvidence(
  pageId: string,
  callId: string,
  signal?: AbortSignal,
): Promise<ToolEvidence> {
  return await requestJSON<ToolEvidence>(
    `/pages/${encodeURIComponent(pageId)}/evidence/${encodeURIComponent(callId)}`,
    {},
    signal,
  );
}

export async function getDiff(pageId: string, signal?: AbortSignal): Promise<DiffResponse> {
  return await requestJSON<DiffResponse>(`/pages/${encodeURIComponent(pageId)}/diff`, {}, signal);
}

export interface EventStreamHandlers {
  onEvent: (event: InvestigationEvent) => void;
  onOpen: () => void;
  // closed is true when the browser gave up instead of scheduling its own retry.
  onError: (closed: boolean) => void;
}

function parseEvent(event: Event): InvestigationEvent | null {
  try {
    const parsed = JSON.parse((event as MessageEvent<string>).data) as unknown;
    if (typeof parsed !== "object" || parsed === null) {
      return null;
    }
    const candidate = parsed as Partial<InvestigationEvent>;
    if (
      typeof candidate.seq !== "number" ||
      typeof candidate.pageId !== "string" ||
      typeof candidate.turnId !== "string" ||
      typeof candidate.type !== "string" ||
      typeof candidate.time !== "string" ||
      typeof candidate.data !== "object" ||
      candidate.data === null
    ) {
      return null;
    }
    return candidate as InvestigationEvent;
  } catch {
    // One malformed frame is dropped rather than tearing down the stream.
    return null;
  }
}

// subscribeEvents replays from the durable cursor and then follows the ordered stream. EventSource sends
// Last-Event-ID automatically after a dropped connection, and the reducer ignores any replayed sequence,
// so reconnecting can neither lose nor duplicate an attempt.
export function subscribeEvents(
  pageId: string,
  after: number,
  handlers: EventStreamHandlers,
): () => void {
  const source = new EventSource(
    `${base}/pages/${encodeURIComponent(pageId)}/events?after=${String(after)}`,
    { withCredentials: true },
  );
  let closed = false;

  const close = () => {
    if (!closed) {
      closed = true;
      source.close();
    }
  };

  const receive = (event: Event) => {
    const parsed = parseEvent(event);
    if (parsed !== null) {
      handlers.onEvent(parsed);
    }
  };

  source.onmessage = receive;
  // Tolerate a server that names the event after its type as well as one that sends the default message
  // event: both carry the same JSON object, and the sequence reducer removes any duplicate.
  for (const type of [
    "turn.started",
    "tool.started",
    "tool.finished",
    "answer",
    "proposal",
    "turn.finished",
  ]) {
    source.addEventListener(type, receive);
  }
  source.onopen = () => {
    handlers.onOpen();
  };
  source.onerror = () => {
    if (source.readyState === EventSource.CLOSED) {
      handlers.onError(true);
      close();
      return;
    }
    handlers.onError(false);
  };

  return close;
}
