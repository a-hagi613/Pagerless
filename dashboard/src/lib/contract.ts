// The investigation wire contract, as specified by docs/investigation-api.md. Every field here is read
// from pagerless-server; the dashboard never invents a successful response. Optional fields are tolerated
// so a newer server can add one without breaking an older embedded build.

export type TurnStatus = "running" | "completed" | "stopped" | "failed" | "interrupted";

export type PageStatus = TurnStatus;

export type MessageRole = "user" | "assistant";

// A citation points at one tool attempt. `pointer` is an optional JSON pointer inside that attempt's
// result; jsonPointer is accepted as an alias because both spellings appear in the specification prose.
export interface Citation {
  callId: string;
  pointer?: string;
  jsonPointer?: string;
}

export interface PageMessage {
  id: string;
  turnId: string;
  role: MessageRole;
  content: string;
  createdAt: string;
  citations: Citation[];
}

export interface PageSummary {
  id: string;
  title: string;
  status: PageStatus;
  updatedAt: string;
  activeTurnId: string | null;
  // Optional hints an implementation may echo back. They are display-only when present.
  namespace?: string;
  target?: string;
}

export interface PageDetail extends PageSummary {
  messages: PageMessage[];
  lastEventId: number;
}

export interface PagesResponse {
  pages: PageSummary[];
}

export interface TurnAccepted {
  pageId: string;
  turnId: string;
}

export interface StopAnswer {
  status: string;
}

export type InvestigationEventType =
  | "turn.started"
  | "tool.started"
  | "tool.finished"
  | "answer"
  | "proposal"
  | "turn.finished";

export interface InvestigationEvent {
  seq: number;
  pageId: string;
  turnId: string;
  type: InvestigationEventType | string;
  time: string;
  data: Record<string, unknown>;
}

export interface EventsResponse {
  events: InvestigationEvent[];
}

export interface ToolEvidence {
  callId: string;
  tool: string;
  arguments: unknown;
  result: unknown;
  status: string;
  durationMs: number;
  redacted: boolean;
  truncated: boolean;
}

export interface DiffFile {
  path: string;
  diff: string;
}

export interface DiffResponse {
  files: DiffFile[];
  prUrl?: string;
}

// A resource reference hint. The server validates kind and name; namespace is optional because the
// reference may already be unique. The dashboard sends the shape documented for create and reply.
export interface InvestigationTarget {
  kind: string;
  name: string;
  namespace?: string;
}

export interface InvestigationRequest {
  message: string;
  namespace?: string;
  target?: InvestigationTarget;
}

export type CreateInvestigation = InvestigationRequest;

export type Reply = InvestigationRequest;

export function targetLabel(target: InvestigationTarget | undefined): string {
  if (target === undefined) {
    return "";
  }
  const parts = [target.kind, target.namespace ?? "", target.name].filter((part) => part !== "");
  return parts.join("/");
}

export function citationPointer(citation: Citation): string | undefined {
  return citation.pointer ?? citation.jsonPointer;
}

export function citationKey(citation: Citation): string {
  return `${citation.callId}|${citationPointer(citation) ?? ""}`;
}
