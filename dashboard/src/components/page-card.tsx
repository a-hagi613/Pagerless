import { ChevronDown, ExternalLink, GitPullRequest, ShieldAlert, SquareTerminal } from "lucide-react";
import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DiffView } from "@/components/diff-view";
import { LiveLabel, NamespaceChip, PhasePill, SeverityPill } from "@/components/pills";
import { TraceStream } from "@/components/trace-stream";
import {
  ageLabel,
  isLive,
  pageSeverity,
  pageSignal,
  pageTitle,
  phaseLabel,
  shortHash,
  targetLabel,
  timestamp,
} from "@/lib/format";
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
  const proposal = detail?.proposal ?? null;
  const proposalHash = proposal?.hash ?? page.proposalHash;
  const severity = pageSeverity(page);
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
              severity === "critical" ? "bg-destructive" : severity === "warning" ? "bg-warning" : "bg-info",
            )}
            aria-hidden="true"
          />
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-1.5">
              <SeverityPill severity={severity} />
              <span className="font-mono text-xs text-muted-foreground">{page.name}</span>
              <NamespaceChip namespace={page.target.namespace} />
              <PhasePill phase={page.phase} />
              {isLive(page.phase) && <LiveLabel label={page.phase === "Verifying" ? "verifying" : "diagnosing"} />}
            </div>

            <h3 id={`page-title-${page.name}`} className="mt-1 text-base leading-snug font-medium text-balance text-foreground">
              {pageTitle(page)}
            </h3>
            <p className="mt-0.5 text-sm text-muted-foreground">{pageSignal(page)}</p>

            <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
              <span className="truncate font-mono">{targetLabel(page.target)}</span>
              <span>{ageLabel(page.createdAt)} ago</span>
              <span>
                {(detail?.triggerCount ?? 1) === 1 ? "1 trigger" : `${String(detail?.triggerCount ?? 1)} triggers`}
              </span>
              {page.pullRequest?.url !== undefined && (
                <a
                  href={page.pullRequest.url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-mint underline-offset-2 hover:underline"
                >
                  PR #{page.pullRequest.number} {page.pullRequest.state?.toLowerCase()}
                  <ExternalLink className="size-3" aria-hidden="true" />
                </a>
              )}
            </div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-1.5 sm:justify-end">
          <Button variant="outline" size="sm" aria-expanded={expanded} aria-controls={regionId} onClick={onToggle}>
            {expanded
              ? "Close"
              : proposalHash === undefined
                ? isLive(page.phase)
                  ? "Watch trace"
                  : "Details"
                : "Review diff"}
            <ChevronDown className={cn("size-3.5 transition-transform", expanded && "rotate-180")} aria-hidden="true" />
          </Button>
          {page.pullRequest?.url !== undefined ? (
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
                  <p className="mt-1 text-sm leading-relaxed text-foreground" data-testid="diagnosis">
                    {detail?.diagnosis !== undefined && detail.diagnosis !== ""
                      ? detail.diagnosis
                      : streaming
                        ? "The Diagnosis arrives when the investigation completes."
                        : "No Diagnosis was recorded for this Page."}
                  </p>
                  <p className="mt-1 font-mono text-xs text-muted-foreground">
                    opened {timestamp(page.createdAt)} · {page.trigger.source}
                    {page.trigger.pod === undefined ? "" : ` · pod ${page.trigger.pod}`}
                  </p>
                  {detail?.hint !== undefined && detail.hint !== "" && (
                    <p className="mt-1 rounded-md border border-border-subtle bg-muted px-2 py-1 text-xs text-muted-foreground">
                      <span className="font-semibold text-foreground">Hint:</span> {detail.hint}
                    </p>
                  )}
                </div>
                <TraceStream steps={steps} streaming={streaming} findings={detail?.findings} />
              </div>

              <div className="flex min-w-0 flex-col gap-3">
                {proposal === null ? (
                  <div className="rounded-lg border border-dashed border-border px-3 py-8 text-center">
                    <p className="font-mono text-sm text-muted-foreground">no Proposal yet</p>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {streaming ? "the trace above is still streaming" : "the Page closed without a file change"}
                    </p>
                  </div>
                ) : (
                  <DiffView proposal={proposal} />
                )}

                <div className="flex flex-col gap-2 border-t border-border-subtle pt-3">
                  <div className="flex flex-wrap items-center gap-2">
                    {proposalHash !== undefined && (
                      <Badge variant="muted" className="font-mono">
                        proposal sha256:{shortHash(proposalHash)}
                      </Badge>
                    )}
                    <span className="ml-auto text-xs text-muted-foreground">
                      {canApprove
                        ? "The decision is recorded against this session"
                        : "This session cannot decide a Proposal"}
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
                      <GitPullRequest className="size-3.5" aria-hidden="true" />{" "}
                      {proposal?.action === "NoAction" ? "Acknowledge Diagnosis" : "Approve & create PR"}
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={!canDecide || reason.trim() === ""}
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
                    Reason (required to dismiss, recorded on the Page)
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
                      A decision is available while the Page is Proposed; this Page is {phaseLabel(page.phase)}.
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

                {detail?.decision !== undefined && (
                  <div className="rounded-lg border border-border-subtle bg-card px-3 py-2 text-sm">
                    <p className="font-medium text-foreground">
                      {detail.decision.outcome} by <span className="font-mono">{detail.decision.decidedBy}</span>
                    </p>
                    <p className="text-xs text-muted-foreground">{timestamp(detail.decision.decidedAt)}</p>
                    {detail.decision.reason !== undefined && detail.decision.reason !== "" && (
                      <p className="mt-1 text-xs text-muted-foreground">{detail.decision.reason}</p>
                    )}
                  </div>
                )}

                {detail?.verification !== undefined && (
                  <div className="rounded-lg border border-border-subtle bg-card px-3 py-2">
                    <p className="font-mono text-xs tracking-[0.12em] text-muted-foreground uppercase">Verification</p>
                    {detail.verification.checks.length === 0 ? (
                      <p className="mt-1 text-xs text-muted-foreground">Still observing the Target.</p>
                    ) : (
                      <ul className="mt-1 flex flex-col gap-1">
                        {detail.verification.checks.map((check) => (
                          <li key={check.name} className="flex items-start gap-1.5 text-xs">
                            <Badge variant={check.passed ? "success" : "danger"}>{check.passed ? "pass" : "fail"}</Badge>
                            <span className="min-w-0 text-muted-foreground">
                              <span className="text-foreground">{check.name}</span>
                              {check.message === undefined ? "" : ` — ${check.message}`}
                            </span>
                          </li>
                        ))}
                      </ul>
                    )}
                  </div>
                )}

                {detail?.narrative !== undefined && detail.narrative !== "" && (
                  <div className="rounded-lg border border-border-subtle bg-card px-3 py-2">
                    <p className="font-mono text-xs tracking-[0.12em] text-muted-foreground uppercase">Narrative</p>
                    <p className="mt-1 text-sm leading-relaxed whitespace-pre-wrap text-foreground">{detail.narrative}</p>
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      )}
    </article>
  );
}
