import { RefreshCw, Square } from "lucide-react";

import { InlineAlert } from "@/components/inline-alert";
import { StatusPill } from "@/components/status-pill";
import { Button } from "@/components/ui/button";
import { timeOfDay } from "@/lib/format";
import type { Investigation } from "@/lib/use-investigation";

export function TurnFooter({
  turn,
  latest,
  hasProposal,
}: {
  turn: Investigation["turns"][number];
  latest: boolean;
  hasProposal: boolean;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 px-1 text-xs text-muted-foreground">
      <StatusPill status={turn.status} />
      <span>
        Turn {turn.index}
        {turn.finishedAt === undefined ? " in progress" : ` finished ${timeOfDay(turn.finishedAt)}`}
      </span>
      {turn.status === "interrupted" && (
        <span>This turn was interrupted by a restart. Its recorded attempts above remain evidence.</span>
      )}
      {turn.status === "completed" && !hasProposal && latest && <span>No source change was proposed.</span>}
    </div>
  );
}

export function StreamNotice({ investigation }: { investigation: Investigation }) {
  const { timeline, activeTurn, reconnect } = investigation;
  if (timeline.gap) {
    return (
      <InlineAlert
        tone="warning"
        title="Some events may be missing"
        action={
          <Button size="xs" variant="outline" onClick={reconnect}>
            Reconnect
          </Button>
        }
      >
        The event sequence jumped. Reconnect from the last recorded event to restore the full activity.
      </InlineAlert>
    );
  }
  if (timeline.stream === "reconnecting") {
    return (
      <p role="status" className="rounded-md border border-info/40 bg-info-surface px-3 py-2 text-sm text-info">
        Reconnecting to the event stream. Attempts already shown stay in place.
      </p>
    );
  }
  if (timeline.stream === "closed" && activeTurn !== null) {
    return (
      <InlineAlert
        tone="warning"
        title="The event stream is disconnected"
        action={
          <Button size="xs" variant="outline" onClick={reconnect}>
            <RefreshCw className="size-3" aria-hidden="true" />
            Reconnect
          </Button>
        }
      >
        The turn may still be running. Reconnect to replay from the last durable event.
      </InlineAlert>
    );
  }
  return null;
}

export function StopControl({ investigation }: { investigation: Investigation }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <p role="status" className="text-sm text-muted-foreground">
        The investigation is running. Stop it before sending a follow-up.
      </p>
      <Button
        variant="destructive"
        size="sm"
        disabled={investigation.stopping}
        onClick={() => {
          void investigation.stop();
        }}
      >
        <Square className="size-3.5" aria-hidden="true" />
        {investigation.stopping ? "Stopping…" : "Stop investigation"}
      </Button>
      {investigation.stopError !== null && (
        <InlineAlert
          title="The stop request failed"
          action={
            <Button size="xs" variant="outline" onClick={() => void investigation.stop()}>
              Try again
            </Button>
          }
        >
          {investigation.stopError}
        </InlineAlert>
      )}
    </div>
  );
}
