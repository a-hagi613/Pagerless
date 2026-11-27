import { useCallback } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";

import { ChatShell } from "@/components/chat-shell";
import { Composer } from "@/components/composer";
import { Conversation } from "@/components/conversation";
import { InlineAlert } from "@/components/inline-alert";
import { PromptChip } from "@/components/brand";
import { Button } from "@/components/ui/button";
import { ApiError, createPage, UnauthenticatedError } from "@/lib/api";
import type { InvestigationRequest } from "@/lib/contract";
import { useAuth } from "@/lib/use-auth";
import { useInvestigation } from "@/lib/use-investigation";
function NewInvestigation() {
  const navigate = useNavigate();
  const { refreshHistory } = useAuth();

  const submit = useCallback(
    async (request: InvestigationRequest, idempotencyKey: string): Promise<string | null> => {
      try {
        const accepted = await createPage(request, idempotencyKey);
        await refreshHistory(true);
        navigate(`/pages/${encodeURIComponent(accepted.pageId)}`, { state: { created: true } });
        return null;
      } catch (cause) {
        if (cause instanceof UnauthenticatedError) {
          return "The session has ended. Sign in again to continue.";
        }
        if (cause instanceof ApiError && cause.status === 409) {
          return `An investigation is already active here: ${cause.message}`;
        }
        return cause instanceof Error ? cause.message : String(cause);
      }
    },
    [navigate, refreshHistory],
  );

  return (
    <ChatShell>
      <div className="mx-auto w-full max-w-3xl px-3 py-8 sm:px-5 sm:py-12">
        <header className="mb-6 flex flex-col gap-2">
          <span className="flex items-center gap-2 font-mono text-xs tracking-[0.16em] text-muted-foreground uppercase">
            <PromptChip />
            new investigation
          </span>
          <h1 className="text-2xl leading-tight font-semibold tracking-tight text-foreground sm:text-3xl">
            Ask the cluster a question
          </h1>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            Start with a symptom, an error, or an open question. Pagerless reads approved cluster and
            repository sources, cites what it finds, and proposes a draft change only when the evidence
            supports one.
          </p>
        </header>

        <section aria-label="New investigation" className="rounded-xl border border-border bg-card p-3 sm:p-4">
          <Composer
            mode="create"
            placeholder="Why is checkout timing out?"
            submitLabel="Start investigation"
            pendingLabel="Starting investigation…"
            onSubmit={submit}
          />
        </section>

        <p className="mt-4 text-xs leading-relaxed text-muted-foreground">
          A namespace and resource reference are optional hints; a question with no failing object is a valid
          investigation. Everyone authorized for this installation shares its history.
        </p>
      </div>
    </ChatShell>
  );
}

function ConversationPage({ pageId }: { pageId: string }) {
  const { refreshHistory } = useAuth();
  const onSettled = useCallback(() => {
    void refreshHistory(true);
  }, [refreshHistory]);
  const investigation = useInvestigation(pageId, onSettled);
  const location = useLocation();
  const focusHeading = (location.state as { created?: boolean } | null)?.created === true;

  return (
    <ChatShell>
      <div className="mx-auto w-full max-w-3xl px-3 py-8 sm:px-5">
        {investigation.timeline.loading && investigation.timeline.detail === null ? (
          <div role="status" aria-busy="true" className="flex flex-col gap-3">
            <span className="sr-only">Loading investigation</span>
            <div className="h-7 w-2/3 animate-pulse rounded bg-muted" />
            <div className="h-24 animate-pulse rounded-lg bg-muted" />
            <div className="h-40 animate-pulse rounded-lg bg-muted" />
          </div>
        ) : investigation.timeline.error !== null && investigation.timeline.detail === null ? (
          <div className="flex flex-col gap-3">
            <h1 className="text-xl font-semibold text-foreground">This investigation could not be opened</h1>
            <InlineAlert
              title="The conversation could not be loaded"
              action={
                <Button size="sm" variant="outline" onClick={investigation.retryLoad}>
                  Try again
                </Button>
              }
            >
              {investigation.timeline.error}
            </InlineAlert>
            <Button asChild variant="ghost" size="sm" className="w-fit">
              <Link to="/">Start a new investigation</Link>
            </Button>
          </div>
        ) : investigation.timeline.detail !== null ? (
          <Conversation
            investigation={investigation}
            page={investigation.timeline.detail}
            focusHeading={focusHeading}
          />
        ) : null}
      </div>
    </ChatShell>
  );
}

// `/` starts a new investigation; `/pages/:pageId` resumes one. The detail component is keyed by page so
// its stream, cursor and errors never leak into another conversation.
export function ChatPage() {
  const { pageId } = useParams<{ pageId: string }>();
  if (pageId === undefined) {
    return <NewInvestigation />;
  }
  return <ConversationPage key={pageId} pageId={pageId} />;
}
