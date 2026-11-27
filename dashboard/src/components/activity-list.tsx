import { CircleCheck, CircleSlash, CircleX, Clock, FileSearch, LoaderCircle, TriangleAlert } from "lucide-react";

import { OutcomePill } from "@/components/status-pill";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { ToolAttempt, TurnSummary } from "@/lib/events";
import { elapsedLabel, timeOfDay, toolStatusTone, type StatusTone } from "@/lib/format";
import { cn } from "@/lib/utils";

const icons: Record<StatusTone, typeof CircleCheck> = {
  success: CircleCheck,
  warning: TriangleAlert,
  danger: CircleX,
  info: LoaderCircle,
  muted: CircleSlash,
};

function argumentsText(value: unknown): string | null {
  if (value === undefined) {
    return null;
  }
  if (typeof value === "string") {
    return value;
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

function AttemptRow({ attempt, onEvidence }: { attempt: ToolAttempt; onEvidence: (callId: string) => void }) {
  const tone = toolStatusTone(attempt.status);
  const Icon = icons[tone];
  const args = argumentsText(attempt.arguments);
  return (
    <li className="rounded-md border border-border-subtle bg-panel px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <Icon
          className={cn("size-3.5 shrink-0", attempt.status === "running" && "motion-safe:animate-spin")}
          aria-hidden="true"
          data-tone={tone}
        />
        <span className="font-mono text-xs font-semibold text-foreground">{attempt.tool}</span>
        <OutcomePill status={attempt.status} />
        {attempt.durationMs !== undefined && (
          <span className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
            <Clock className="size-3" aria-hidden="true" />
            {elapsedLabel(attempt.durationMs)}
          </span>
        )}
        {attempt.redacted === true && <Badge variant="warning">redacted</Badge>}
        {attempt.truncated === true && <Badge variant="warning">truncated</Badge>}
        <span className="ml-auto font-mono text-xs text-muted-foreground" title={attempt.startedAt}>
          {timeOfDay(attempt.startedAt)}
        </span>
      </div>

      <div className="mt-1 flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs text-muted-foreground">{attempt.callId}</span>
        <Button
          variant="ghost"
          size="xs"
          onClick={() => {
            onEvidence(attempt.callId);
          }}
          aria-label={`Read evidence for ${attempt.tool} attempt ${attempt.callId}`}
        >
          <FileSearch className="size-3" aria-hidden="true" />
          Evidence
        </Button>
        {args !== null && (
          <details className="min-w-0">
            <summary className="cursor-pointer text-xs font-medium text-muted-foreground hover:text-foreground">
              Sanitized arguments
            </summary>
            <pre className="mt-1 max-h-40 overflow-auto rounded border border-border-subtle bg-terminal p-2 font-mono text-xs whitespace-pre-wrap text-terminal-foreground">
              {args}
            </pre>
          </details>
        )}
      </div>
    </li>
  );
}

// Every attempt in the turn is listed in sequence. A denial, a truncation and a retry are separate rows
// with their own call IDs; nothing is merged by tool name or time.
export function ActivityList({
  turn,
  onEvidence,
}: {
  turn: TurnSummary;
  onEvidence: (callId: string) => void;
}) {
  return (
    <section
      aria-label={`Tool activity for turn ${String(turn.index)}`}
      className="rounded-lg border border-border-subtle bg-card p-3"
      data-testid="activity"
    >
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <h2 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Activity</h2>
        <span className="font-mono text-xs text-muted-foreground">
          {turn.attempts.length === 1 ? "1 attempt" : `${String(turn.attempts.length)} attempts`}
        </span>
        <span className="ml-auto text-xs text-muted-foreground" title={turn.startedAt}>
          {turn.startedAt === undefined ? "" : `started ${timeOfDay(turn.startedAt)}`}
        </span>
      </div>

      {turn.attempts.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No tool calls were recorded in this turn.
        </p>
      ) : (
        <ol className="space-y-1.5">
          {turn.attempts.map((attempt) => (
            <AttemptRow key={`${attempt.callId}-${String(attempt.startSeq)}`} attempt={attempt} onEvidence={onEvidence} />
          ))}
        </ol>
      )}
    </section>
  );
}
