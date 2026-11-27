// Pure timeline logic for one investigation. The server owns sequence, ordering and sanitization; this
// module only folds the append-only event log into what the conversation renders. Keeping it pure is what
// makes replay, duplicate suppression and the "every attempt is visible" rule testable without a browser.

import type {
  Citation,
  InvestigationEvent,
  PageDetail,
  PageMessage,
  PageStatus,
  TurnStatus,
} from "./contract";

export type StreamStatus = "idle" | "connecting" | "live" | "reconnecting" | "closed";

export interface ToolAttempt {
  callId: string;
  tool: string;
  startSeq: number;
  startedAt: string;
  finishSeq?: number;
  finishedAt?: string;
  status: string;
  durationMs?: number;
  arguments?: unknown;
  redacted?: boolean;
  truncated?: boolean;
}

export interface AnswerSummary {
  seq: number;
  turnId: string;
  time: string;
  content: string;
  citations: Citation[];
  messageId?: string;
}

export interface ProposalSummary {
  seq: number;
  turnId: string;
  time: string;
  baseSha?: string;
  prUrl?: string;
  filePaths: string[];
}

export interface TurnSummary {
  turnId: string;
  index: number;
  status: TurnStatus;
  startedAt?: string;
  finishedAt?: string;
  attempts: ToolAttempt[];
  answer?: AnswerSummary;
  proposal?: ProposalSummary;
}

export interface TimelineState {
  detail: PageDetail | null;
  events: InvestigationEvent[];
  lastSeq: number;
  stream: StreamStatus;
  loading: boolean;
  error: string | null;
  gap: boolean;
}

export type TimelineAction =
  | { type: "loading" }
  | { type: "detail"; detail: PageDetail }
  | { type: "failed"; message: string }
  | { type: "event"; event: InvestigationEvent }
  | { type: "replay"; events: InvestigationEvent[] }
  | { type: "stream"; status: StreamStatus }
  | { type: "gap"; gap: boolean }
  | { type: "message"; message: PageMessage };

export const initialTimeline: TimelineState = {
  detail: null,
  events: [],
  lastSeq: 0,
  stream: "idle",
  loading: true,
  error: null,
  gap: false,
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function optionalNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function optionalBoolean(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

export function parseCitations(value: unknown): Citation[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const citations: Citation[] = [];
  for (const entry of value) {
    if (!isRecord(entry) || typeof entry.callId !== "string" || entry.callId === "") {
      continue;
    }
    citations.push({
      callId: entry.callId,
      ...(typeof entry.pointer === "string" ? { pointer: entry.pointer } : {}),
      ...(typeof entry.jsonPointer === "string" ? { jsonPointer: entry.jsonPointer } : {}),
    });
  }
  return citations;
}

function validEvent(event: InvestigationEvent): boolean {
  return (
    Number.isInteger(event.seq) &&
    event.seq >= 1 &&
    typeof event.pageId === "string" &&
    typeof event.turnId === "string" &&
    typeof event.type === "string" &&
    typeof event.time === "string" &&
    isRecord(event.data)
  );
}

// mergeEvents folds a batch into the existing sequence. An event already seen by sequence number is
// dropped, so a reconnect that replays the tail never duplicates an attempt. A jump in the sequence marks
// the timeline as incomplete instead of silently pretending it is whole.
function mergeEvents(
  existing: readonly InvestigationEvent[],
  incoming: readonly InvestigationEvent[],
  lastSeq: number,
): { events: InvestigationEvent[]; lastSeq: number; gap: boolean } {
  let cursor = lastSeq;
  let gap = false;
  const additions: InvestigationEvent[] = [];
  const ordered = [...incoming].sort((left, right) => left.seq - right.seq);
  for (const event of ordered) {
    if (!validEvent(event) || event.seq <= cursor) {
      continue;
    }
    if (event.seq > cursor + 1) {
      gap = true;
    }
    additions.push(event);
    cursor = event.seq;
  }
  additions.sort((left, right) => left.seq - right.seq);
  return { events: [...existing, ...additions], lastSeq: cursor, gap };
}

export function timelineReducer(state: TimelineState, action: TimelineAction): TimelineState {
  switch (action.type) {
    case "loading":
      return { ...state, loading: true, error: null };
    case "detail": {
      // A detail read carries lastEventId as a replay hint, not as a promise that every event before it
      // has been folded into this state. Advancing the cursor here could drop a terminal event the stream
      // is about to deliver, so the cursor only moves when an event is actually accepted.
      return {
        ...state,
        detail: action.detail,
        loading: false,
        error: null,
      };
    }
    case "failed":
      return { ...state, loading: false, error: action.message };
    case "event": {
      const merged = mergeEvents(state.events, [action.event], state.lastSeq);
      return { ...state, events: merged.events, lastSeq: merged.lastSeq, gap: state.gap || merged.gap };
    }
    case "replay": {
      const merged = mergeEvents(state.events, action.events, state.lastSeq);
      return { ...state, events: merged.events, lastSeq: merged.lastSeq, gap: merged.gap };
    }
    case "stream":
      return { ...state, stream: action.status };
    case "gap":
      return { ...state, gap: action.gap };
    case "message": {
      if (state.detail === null) {
        return state;
      }
      return { ...state, detail: { ...state.detail, messages: [...state.detail.messages, action.message] } };
    }
    default:
      return state;
  }
}

function attemptFromStart(event: InvestigationEvent): ToolAttempt | null {
  const { callId, tool } = event.data;
  if (typeof callId !== "string" || typeof tool !== "string") {
    return null;
  }
  return {
    callId,
    tool,
    startSeq: event.seq,
    startedAt: event.time,
    status: "running",
    ...(event.data.arguments !== undefined ? { arguments: event.data.arguments } : {}),
  };
}

function finishAttempt(event: InvestigationEvent): ToolAttempt | null {
  const { callId, tool } = event.data;
  if (typeof callId !== "string") {
    return null;
  }
  return {
    callId,
    tool: typeof tool === "string" ? tool : "tool",
    startSeq: event.seq,
    startedAt: event.time,
    finishSeq: event.seq,
    finishedAt: event.time,
    status: typeof event.data.status === "string" ? event.data.status : "completed",
    ...(optionalNumber(event.data.durationMs) !== undefined
      ? { durationMs: optionalNumber(event.data.durationMs) }
      : {}),
    ...(event.data.arguments !== undefined ? { arguments: event.data.arguments } : {}),
    ...(optionalBoolean(event.data.redacted) !== undefined
      ? { redacted: optionalBoolean(event.data.redacted) }
      : {}),
    ...(optionalBoolean(event.data.truncated) !== undefined
      ? { truncated: optionalBoolean(event.data.truncated) }
      : {}),
  };
}

// coerceEvents sorts defensively: the wire promises order, and an out-of-order batch must not reorder the
// transcript. Unknown event types remain in the log but contribute no attempt.
function sorted(events: readonly InvestigationEvent[]): InvestigationEvent[] {
  return [...events].sort((left, right) => left.seq - right.seq);
}

export function toolAttempts(events: readonly InvestigationEvent[]): ToolAttempt[] {
  const attempts: ToolAttempt[] = [];
  const byCall = new Map<string, ToolAttempt>();
  for (const event of sorted(events)) {
    if (event.type === "tool.started") {
      const attempt = attemptFromStart(event);
      if (attempt !== null) {
        attempts.push(attempt);
        byCall.set(attempt.callId, attempt);
      }
      continue;
    }
    if (event.type === "tool.finished") {
      const finished = finishAttempt(event);
      if (finished === null) {
        continue;
      }
      const started = byCall.get(finished.callId);
      if (started === undefined) {
        // A finished attempt whose start precedes the reader's cursor is still shown: hiding it would
        // hide a denial or a retry from the operator.
        attempts.push(finished);
        byCall.set(finished.callId, finished);
        continue;
      }
      started.finishSeq = finished.finishSeq;
      started.finishedAt = finished.finishedAt;
      started.status = finished.status;
      if (finished.durationMs !== undefined) {
        started.durationMs = finished.durationMs;
      }
      if (finished.redacted !== undefined) {
        started.redacted = finished.redacted;
      }
      if (finished.truncated !== undefined) {
        started.truncated = finished.truncated;
      }
    }
  }
  return attempts;
}

function answerFromEvent(event: InvestigationEvent): AnswerSummary | null {
  const content = optionalString(event.data.content);
  if (content === undefined) {
    return null;
  }
  return {
    seq: event.seq,
    turnId: event.turnId,
    time: event.time,
    content,
    citations: parseCitations(event.data.citations),
    ...(optionalString(event.data.messageId) !== undefined
      ? { messageId: optionalString(event.data.messageId) }
      : {}),
  };
}

function proposalFromEvent(event: InvestigationEvent): ProposalSummary {
  const files = Array.isArray(event.data.files) ? event.data.files : [];
  const filePaths = files.flatMap((file) => {
    if (!isRecord(file) || typeof file.path !== "string") {
      return [];
    }
    return [file.path];
  });
  return {
    seq: event.seq,
    turnId: event.turnId,
    time: event.time,
    ...(optionalString(event.data.baseSha) !== undefined
      ? { baseSha: optionalString(event.data.baseSha) }
      : {}),
    ...(optionalString(event.data.prUrl) !== undefined ? { prUrl: optionalString(event.data.prUrl) } : {}),
    filePaths,
  };
}

export function turnSummaries(events: readonly InvestigationEvent[]): TurnSummary[] {
  const turns: TurnSummary[] = [];
  const byId = new Map<string, TurnSummary>();
  for (const event of sorted(events)) {
    let turn = byId.get(event.turnId);
    if (turn === undefined) {
      turn = {
        turnId: event.turnId,
        index: turns.length + 1,
        status: "running",
        attempts: [],
      };
      turns.push(turn);
      byId.set(event.turnId, turn);
    }
    if (event.type === "turn.started") {
      turn.startedAt = event.time;
      turn.status = "running";
    } else if (event.type === "tool.started") {
      const attempt = attemptFromStart(event);
      if (attempt !== null) {
        turn.attempts.push(attempt);
      }
    } else if (event.type === "tool.finished") {
      const finished = finishAttempt(event);
      if (finished !== null) {
        const started = turn.attempts.find((attempt) => attempt.callId === finished.callId);
        if (started === undefined) {
          turn.attempts.push(finished);
        } else {
          started.finishSeq = finished.finishSeq;
          started.finishedAt = finished.finishedAt;
          started.status = finished.status;
          if (finished.durationMs !== undefined) {
            started.durationMs = finished.durationMs;
          }
          if (finished.redacted !== undefined) {
            started.redacted = finished.redacted;
          }
          if (finished.truncated !== undefined) {
            started.truncated = finished.truncated;
          }
        }
      }
    } else if (event.type === "answer") {
      const answer = answerFromEvent(event);
      if (answer !== null) {
        turn.answer = answer;
      }
    } else if (event.type === "proposal") {
      turn.proposal = proposalFromEvent(event);
    } else if (event.type === "turn.finished") {
      turn.finishedAt = event.time;
      turn.status = turnStatus(event.data.status);
    }
  }
  return turns;
}

function turnStatus(value: unknown): TurnStatus {
  switch (value) {
    case "completed":
    case "stopped":
    case "failed":
    case "interrupted":
    case "running":
      return value;
    default:
      return "failed";
  }
}

function lastSeqForTurn(events: readonly InvestigationEvent[], turnId: string): number {
  let seq = 0;
  for (const event of events) {
    if (event.turnId === turnId) {
      seq = Math.max(seq, event.seq);
    }
  }
  return seq;
}

// pageStatus prefers the live log over the summary fetched before it. A turn that started and has not
// finished is running, unless a detail read at least as new as that turn's last event says the page has
// no active turn (for example after a stop that arrived while the stream was down).
export function currentStatus(state: TimelineState): PageStatus {
  const turns = turnSummaries(state.events);
  const open = [...turns].reverse().find((turn) => turn.status === "running" && turn.finishedAt === undefined);
  if (open !== undefined) {
    const known = lastSeqForTurn(state.events, open.turnId);
    if (
      state.detail !== null &&
      state.detail.activeTurnId === null &&
      state.detail.lastEventId >= known &&
      isTerminalPageStatus(state.detail.status)
    ) {
      return state.detail.status;
    }
    return "running";
  }
  if (state.detail !== null && state.detail.activeTurnId !== null) {
    const active = turns.find((turn) => turn.turnId === state.detail?.activeTurnId);
    if (active !== undefined) {
      return active.status;
    }
    return "running";
  }
  const finished = [...turns].reverse().find((turn) => turn.finishedAt !== undefined);
  if (finished !== undefined) {
    return finished.status;
  }
  return state.detail?.status ?? "running";
}

function isTerminalPageStatus(status: PageStatus): boolean {
  return status === "completed" || status === "stopped" || status === "failed" || status === "interrupted";
}

export function activeTurnId(state: TimelineState): string | null {
  const turns = turnSummaries(state.events);
  const open = [...turns].reverse().find((turn) => turn.status === "running" && turn.finishedAt === undefined);
  if (open !== undefined) {
    if (
      state.detail !== null &&
      state.detail.activeTurnId === null &&
      state.detail.lastEventId >= lastSeqForTurn(state.events, open.turnId) &&
      isTerminalPageStatus(state.detail.status)
    ) {
      return null;
    }
    return open.turnId;
  }
  if (state.detail !== null && state.detail.activeTurnId !== null) {
    return state.detail.activeTurnId;
  }
  return null;
}

// messagesForPage merges the stored transcript with answers the live stream has shown. A detail read
// always wins by turn and role, so a reconnection that refetches cannot duplicate the answer.
export function messagesForPage(
  detail: PageDetail | null,
  events: readonly InvestigationEvent[],
): PageMessage[] {
  if (detail === null) {
    return [];
  }
  const stored = detail.messages;
  const answered = new Set(
    stored.filter((message) => message.role === "assistant").map((message) => message.turnId),
  );
  const live: PageMessage[] = [];
  for (const event of sorted(events)) {
    if (event.type !== "answer") {
      continue;
    }
    const answer = answerFromEvent(event);
    if (answer === null || answered.has(event.turnId)) {
      continue;
    }
    answered.add(event.turnId);
    live.push({
      id: answer.messageId ?? `${event.turnId}:answer`,
      turnId: event.turnId,
      role: "assistant",
      content: answer.content,
      createdAt: answer.time,
      citations: answer.citations,
    });
  }
  if (live.length === 0) {
    return stored;
  }
  return [...stored, ...live].sort((left, right) => {
    const leftTime = Date.parse(left.createdAt);
    const rightTime = Date.parse(right.createdAt);
    if (Number.isNaN(leftTime) || Number.isNaN(rightTime) || leftTime === rightTime) {
      return 0;
    }
    return leftTime - rightTime;
  });
}

export function latestProposal(events: readonly InvestigationEvent[]): ProposalSummary | null {
  const turns = turnSummaries(events);
  for (let index = turns.length - 1; index >= 0; index -= 1) {
    const proposal = turns[index].proposal;
    if (proposal !== undefined) {
      return proposal;
    }
  }
  return null;
}

export function evidencePointer(citation: Citation): string | undefined {
  return citation.pointer ?? citation.jsonPointer;
}
