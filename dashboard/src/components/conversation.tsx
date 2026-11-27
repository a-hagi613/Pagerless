import { RefreshCw } from "lucide-react";
import { useState, type ReactNode } from "react";

import { ActivityList } from "@/components/activity-list";
import { Composer } from "@/components/composer";
import { EvidenceDialog } from "@/components/evidence-dialog";
import { StreamNotice, StopControl, TurnFooter } from "@/components/conversation-status";
import { InlineAlert } from "@/components/inline-alert";
import { MessageItem } from "@/components/message-item";
import { ProposalPanel } from "@/components/proposal-panel";
import { StatusPill } from "@/components/status-pill";
import { Button } from "@/components/ui/button";
import type { Citation, PageDetail } from "@/lib/contract";
import { conversationTitle, timeOfDay } from "@/lib/format";
import type { Investigation } from "@/lib/use-investigation";
import { cn } from "@/lib/utils";

export function Conversation({
  investigation,
  page,
  focusHeading,
}: {
  investigation: Investigation;
  page: PageDetail;
  focusHeading: boolean;
}) {
  const [evidence, setEvidence] = useState<{ callId: string; pointer?: string } | null>(null);

  const { messages, turns, proposal, status } = investigation;
  const latest = turns.length > 0 ? turns[turns.length - 1] : undefined;
  const turnByMessage = new Map(turns.map((turn) => [turn.turnId, turn]));
  const renderedActivity = new Set<string>();
  const renderedFooter = new Set<string>();
  const renderedProposal = new Set<string>();
  const nodes: ReactNode[] = [];

  const renderActivity = (turn: Investigation["turns"][number]) => {
    if (renderedActivity.has(turn.turnId)) {
      return;
    }
    renderedActivity.add(turn.turnId);
    nodes.push(
      <li key={`activity-${turn.turnId}`} className="min-w-0">
        <ActivityList
          turn={turn}
          onEvidence={(callId) => {
            setEvidence({ callId });
          }}
        />
      </li>,
    );
  };

  const renderClosing = (turn: Investigation["turns"][number], withFooter: boolean) => {
    if (
      turn.proposal !== undefined &&
      proposal !== null &&
      turn.proposal.seq === proposal.seq &&
      !renderedProposal.has(turn.turnId)
    ) {
      renderedProposal.add(turn.turnId);
      nodes.push(
        <li key={`proposal-${turn.turnId}`} className="min-w-0">
          <ProposalPanel
            diff={investigation.diff}
            diffError={investigation.diffError}
            proposal={proposal}
            onRetry={() => {
              void investigation.refreshDiff();
            }}
          />
        </li>,
      );
    }
    if (
      withFooter &&
      !renderedFooter.has(turn.turnId) &&
      (turn.status !== "running" || turn.finishedAt !== undefined)
    ) {
      renderedFooter.add(turn.turnId);
      nodes.push(
        <li key={`footer-${turn.turnId}`} className="min-w-0">
          <TurnFooter turn={turn} latest={turn === latest} hasProposal={turn.proposal !== undefined} />
        </li>,
      );
    }
  };

  for (const message of messages) {
    const turn = turnByMessage.get(message.turnId);
    if (message.role === "assistant" && turn !== undefined) {
      renderActivity(turn);
    }
    nodes.push(
      <MessageItem
        key={message.id}
        message={message}
        onEvidence={(citation: Citation) => {
          setEvidence({ callId: citation.callId, ...(citation.pointer !== undefined ? { pointer: citation.pointer } : {}) });
        }}
      />,
    );
    if (message.role === "assistant" && turn !== undefined) {
      renderClosing(turn, true);
    }
  }

  // A running turn has no answer yet, so its activity belongs at the end of the transcript.
  for (const turn of turns) {
    if (!renderedActivity.has(turn.turnId)) {
      renderActivity(turn);
      renderClosing(turn, true);
    }
  }

  return (
    <div className="flex min-h-[calc(100svh-3.5rem)] flex-col">
      <div className="mx-auto w-full max-w-3xl flex-1 px-3 pt-5 pb-4 sm:px-5">
        <header className="mb-4 flex flex-col gap-2">
          <h1
            autoFocus={focusHeading}
            tabIndex={-1}
            className="text-xl leading-tight font-semibold tracking-tight text-foreground outline-none sm:text-2xl"
          >
            {page.title === "" ? conversationTitle(messages[0]?.content ?? "Investigation") : page.title}
          </h1>
          <div className="flex flex-wrap items-center gap-2">
            <span data-testid="page-status">
              <StatusPill status={status} />
            </span>
            <span className="font-mono text-xs text-muted-foreground" title={page.updatedAt}>
              updated {timeOfDay(page.updatedAt)}
            </span>
            {page.namespace !== undefined && (
              <span className="rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                namespace {page.namespace}
              </span>
            )}
            {page.target !== undefined && (
              <span className="rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                {page.target}
              </span>
            )}
            <span className="sr-only" role="status" aria-live="polite">
              {status === "running" ? "The investigation is running." : `The investigation is ${status}.`}
            </span>
          </div>
        </header>

        <div className="flex flex-col gap-3">
          <StreamNotice investigation={investigation} />
          {investigation.activityError !== null && (
            <InlineAlert
              tone="warning"
              title="Earlier activity could not be loaded"
              action={
                <Button size="xs" variant="outline" onClick={investigation.retryLoad}>
                  <RefreshCw className="size-3" aria-hidden="true" />
                  Retry
                </Button>
              }
            >
              {investigation.activityError}
            </InlineAlert>
          )}

          {messages.length === 0 && turns.length === 0 ? (
            <div className="rounded-lg border border-dashed border-border px-4 py-10 text-center">
              <p className="text-sm font-medium text-foreground">This conversation has no messages yet.</p>
              <p className="mt-1 text-sm text-muted-foreground">Send the first message below.</p>
            </div>
          ) : (
            <ol aria-label="Conversation" className="flex flex-col gap-3">
              {nodes}
            </ol>
          )}
        </div>
      </div>

      <div className={cn("sticky bottom-0 z-20 border-t border-border bg-background/95 backdrop-blur")}>
        <div className="mx-auto w-full max-w-3xl px-3 py-3 sm:px-5">
          {investigation.activeTurn !== null ? (
            <StopControl investigation={investigation} />
          ) : (
            <Composer
              mode="reply"
              placeholder="Ask a follow-up about this investigation."
              submitLabel="Send follow-up"
              pendingLabel="Sending follow-up…"
              onSubmit={(request, key) => investigation.sendReply(request, key)}
            />
          )}
        </div>
      </div>

      <EvidenceDialog
        pageId={investigation.pageId}
        callId={evidence?.callId ?? null}
        pointer={evidence?.pointer}
        open={evidence !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEvidence(null);
          }
        }}
      />
    </div>
  );
}
