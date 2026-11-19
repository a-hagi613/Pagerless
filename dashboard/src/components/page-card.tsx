import { ChevronDown, ExternalLink, GitPullRequest, ShieldAlert, SquareTerminal } from "lucide-react";
import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DiffView } from "@/components/diff-view";
import { LiveLabel, NamespaceChip, PhasePill, SeverityPill } from "@/components/pills";
import { TraceStream } from "@/components/trace-stream";
import { ageLabel, isLive, pageTitle, pageSignal, shortHash, subjectLabel } from "@/lib/format";
import type { PageDetail, PageSummary, Step } from "@/lib/types";
import { cn } from "@/lib/utils";

export interface PageCardProps {
  page: PageSummary;
  detail: PageDetail | null;
  expanded: boolean;
  loading: boolean;
  steps: Step[];
  streaming: boolean;
  canApprove: boolean;
  busy: boolean;
  error: string | null;
  onToggle: () => void;
  onApprove: (page: PageSummary, proposalHash: string) => void;
  onDismiss: (page: PageSummary, proposalHash: string, reason: string) => void;
  onOpenCli: () => void;
}

export function PageCard({
  page,
  detail,
  expanded,
  loading,
  steps,
  streaming,
  canApprove,
  busy,
  error,
  onToggle,
  onApprove,
  onDismiss,
  onOpenCli,
}: PageCardProps) {
  const [reason, setReason] = useState("");
  const proposal = detail?.proposal;
  const proposalHash = proposal?.hash ?? page.proposalHash;
  const canDecide = page.phase === "Proposed" && proposalHash !== undefined && canApprove && !busy;
  const regionId = `page-body-${page.name}`;

  return (
    <article
      id={`page-${page.name}`}
      aria-labelledby={`page-title-${page.name}`}
      className={cn(
        "min-w-0 overflow-hidden rounded-xl border bg-card transition-colors",
        expanded ? "border-primary" : "border-border",
      )}
    >
      <div className="flex flex-col gap-2 px-3 py-2.5 sm:flex-row sm:items-start sm:px-4">
        <div className="flex min-w-0 flex-1 items-start gap-2">
          <span
            className={cn(
              "mt-1 h-9 w-1 shrink-0 rounded-full",
              page.severity === "critical"
                ? "bg-destructive"
                : page.severity === "warning"
                  ? "bg-warning"
                  : "bg-info",
            )}
            aria-hidden="true"
          />
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-1.5">
              <SeverityPill severity={page.severity} />
              <span className="font-mono text-xs text-muted-foreground">{page.name}</span>
              <NamespaceChip namespace={page.subject.namespace} />
              <PhasePill phase={page.phase} />
              {isLive(page.phase) && <LiveLabel label={page.phase === "Verifying" ? "verifying" : "diagnosing"} />}
            </div>

            <h3 id={`page-title-${page.name}`} className="mt-1 text-base leading-snug font-medium text-balance text-foreground">
              {pageTitle(page)}
            </h3>
            <p className="mt-0.5 text-sm text-muted-foreground">{pageSignal(page)}</p>

            <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 font-mono text-xs text-muted-foreground">
              <span className="truncate">{subjectLabel(page.subject)}</span>
              <span>{ageLabel(page.createdAt)} ago</span>
              <span>
                {page.triggerCount} {page.triggerCount === 1 ? "trigger" : "triggers"}
              </span>
              {page.repository !== undefined && (
                <Badge variant={page.repository.confidence === "exact" ? "mint" : "muted"}>
                  {page.repository.confidence} provenance
                </Badge>
              )}
              {page.pullRequest !== undefined && (
                <a
                  href={page.pullRequest.url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-mint underline-offset-2 hover:underline"
                >
                  PR #{page.pullRequest.number} {page.pullRequest.state}
                  <ExternalLink className="size-3" aria-hidden="true" />
                </a>
              )}
            </div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-1.5 sm:justify-end">
          <Button
            variant="outline"
            size="sm"
            aria-expanded={expanded}
            aria-controls={regionId}
            onClick={onToggle}
          >
            {expanded
              ? "Close"
              : proposalHash === undefined
                ? isLive(page.phase)
                  ? "Watch trace"
                  : "Details"
                : "Review diff"}
            <ChevronDown className={cn("size-3.5 transition-transform", expanded && "rotate-180")} aria-hidden="true" />
          </Button>
          {page.pullRequest !== undefined ? (
            <Button asChild size="sm" variant="secondary">
              <a href={page.pullRequest.url} target="_blank" rel="noreferrer">
                PR #{page.pullRequest.number} <ExternalLink className="size-3" aria-hidden="true" />
              </a>
            </Button>
          ) : (
            proposalHash !== undefined && (
              <Button
                size="sm"
                disabled={!canDecide}
                onClick={() => {
                  onApprove(page, proposalHash);
                }}
              >
                <GitPullRequest className="size-3.5" aria-hidden="true" /> Create PR
              </Button>
            )
          )}
        </div>
      </div>

      {expanded && (
        <div id={regionId} className="border-t border-border-subtle bg-panel px-3 py-3 sm:px-4">
          {loading && detail === null ? (
            <p className="text-sm text-muted-foreground">Loading the Page…</p>
          ) : (
            <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
              <div className="flex min-w-0 flex-col gap-3">
                <div className="flex flex-col gap-1">
                  <h4 className="font-mono text-sm tracking-[0.12em] text-muted-foreground uppercase">Root cause</h4>
                  <p className="mt-1 text-sm leading-relaxed text-foreground">
                    {detail?.diagnosis ??
                      (streaming
                        ? "The diagnosis arrives when the investigation completes."
                        : "No diagnosis was recorded for this Page.")}
                  </p>
                  <p className="mt-1 font-mono text-xs text-muted-foreground">
                    opened {page.createdAt} · {page.triggerCount} triggers
                    {page.suppressedCount === undefined ? "" : ` · ${String(page.suppressedCount)} suppressed`}
                  </p>
                </div>
                <TraceStream steps={steps} streaming={streaming} findings={detail?.findings} />
              </div>

              <div className="flex min-w-0 flex-col gap-3">
                {proposal === undefined ? (
                  <div className="rounded-lg border border-dashed border-border px-3 py-8 text-center">
                    <p className="font-mono text-sm text-muted-foreground">no verified proposal yet</p>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {streaming ? "the trace above is still streaming" : "the Page closed without a file change"}
                    </p>
                  </div>
                ) : (
                  <DiffView proposal={proposal} />
                )}

                <div className="flex flex-col gap-2 border-t border-border-subtle pt-3">
                  <div className="flex flex-wrap items-center gap-2">
                    {proposal !== undefined && (
                      <Badge variant="muted" className="font-mono">
                        proposal sha256:{shortHash(proposal.hash)}
                      </Badge>
                    )}
                    <span className="ml-auto text-xs text-muted-foreground">
                      {canApprove ? "Decisions are recorded against your session" : "This session cannot approve"}
                    </span>
                  </div>

                  <div className="flex flex-wrap items-center gap-2">
                    <Button
                      size="sm"
                      disabled={!canDecide}
                      onClick={() => {
                        if (proposalHash !== undefined) {
                          onApprove(page, proposalHash);
                        }
                      }}
                    >
                      <GitPullRequest className="size-3.5" aria-hidden="true" /> Approve &amp; create PR
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={!canDecide}
                      onClick={() => {
                        if (proposalHash !== undefined) {
                          onDismiss(page, proposalHash, reason.trim());
                        }
                      }}
                    >
                      <ShieldAlert className="size-3.5" aria-hidden="true" /> Dismiss
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="ml-auto"
                      onClick={onOpenCli}
                      aria-label="Open the CLI equivalent for this Page"
                    >
                      <SquareTerminal className="size-3.5" aria-hidden="true" /> CLI
                    </Button>
                  </div>

                  <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground" htmlFor={`reason-${page.name}`}>
                    Reason (optional, recorded on the Page)
                    <Input
                      id={`reason-${page.name}`}
                      value={reason}
                      onChange={(event) => {
                        setReason(event.target.value);
                      }}
                      placeholder="handled in another change"
                      className="font-mono text-xs"
                    />
                  </label>

                  {page.phase !== "Proposed" && (
                    <p className="text-xs text-muted-foreground">
                      A decision is available while the Page is Proposed; this Page is {page.phase}.
                    </p>
                  )}
                  {page.phase === "Proposed" && proposalHash === undefined && (
                    <p className="text-xs text-warning">
                      The Proposal hash is not published yet, so the decision controls stay closed.
                    </p>
                  )}
                  {error !== null && (
                    <p role="alert" className="text-sm text-destructive">
                      {error}
                    </p>
                  )}
                </div>
              </div>
            </div>
          )}
        </div>
      )}
    </article>
  );
}
