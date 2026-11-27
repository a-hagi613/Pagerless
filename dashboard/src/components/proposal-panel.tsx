import { ExternalLink, GitPullRequest, RefreshCw } from "lucide-react";

import { InlineAlert } from "@/components/inline-alert";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { DiffResponse } from "@/lib/contract";
import { diffCounts, parseUnifiedDiff, type DiffLine } from "@/lib/diff";
import type { ProposalSummary } from "@/lib/events";
import { safeExternalURL, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";

function DiffBody({ diff }: { diff: string }) {
  const lines: DiffLine[] = parseUnifiedDiff(diff);
  const counts = diffCounts(lines);
  return (
    <div className="min-w-0">
      <p className="mb-1 font-mono text-xs text-muted-foreground">
        +{counts.added} −{counts.removed}
      </p>
      <ol className="max-h-[28rem] overflow-auto rounded-md border border-border-subtle bg-terminal py-1 font-mono text-xs leading-relaxed">
        {lines.map((line, index) => (
          <li
            key={index}
            data-kind={line.kind}
            className={cn(
              "grid min-w-max grid-cols-[1.5rem_minmax(0,1fr)] gap-1 px-2",
              line.kind === "add" && "bg-diff-add-surface text-diff-add-foreground",
              line.kind === "remove" && "bg-diff-remove-surface text-diff-remove-foreground",
              line.kind === "context" && "text-terminal-foreground",
              line.kind === "meta" && "text-terminal-muted",
            )}
          >
            <span aria-hidden="true" className="select-none">
              {line.kind === "add" ? "+" : line.kind === "remove" ? "−" : line.kind === "context" ? " " : ""}
            </span>
            <span className="whitespace-pre">
              <span className="sr-only">
                {line.kind === "add" ? "added: " : line.kind === "remove" ? "removed: " : ""}
              </span>
              {line.text}
            </span>
          </li>
        ))}
      </ol>
    </div>
  );
}

// The proposal is shown only from the server's diff response. An empty file list is the documented "no
// proposal" answer, so the card disappears rather than inventing a change.
export function ProposalPanel({
  diff,
  diffError,
  proposal,
  onRetry,
}: {
  diff: DiffResponse | null;
  diffError: string | null;
  proposal: ProposalSummary | null;
  onRetry: () => void;
}) {
  const prUrl = safeExternalURL(diff?.prUrl ?? proposal?.prUrl);
  const files = diff?.files ?? [];

  if (proposal === null && files.length === 0 && prUrl === undefined) {
    return null;
  }

  return (
    <section
      aria-label="Proposed change"
      className="flex min-w-0 flex-col gap-3 rounded-lg border border-border bg-card p-3"
      data-testid="proposal"
    >
      <div className="flex flex-wrap items-center gap-2">
        <GitPullRequest className="size-4 text-mint" aria-hidden="true" />
        <h2 className="text-sm font-semibold text-foreground">Proposed change</h2>
        {proposal?.baseSha !== undefined && (
          <span className="font-mono text-xs text-muted-foreground">base {shortId(proposal.baseSha)}</span>
        )}
        {prUrl !== undefined && (
          <Button asChild size="sm" variant="outline" className="ml-auto">
            <a href={prUrl} target="_blank" rel="noreferrer">
              <ExternalLink className="size-3.5" aria-hidden="true" />
              Open draft pull request
            </a>
          </Button>
        )}
      </div>

      {diffError !== null && (
        <InlineAlert
          tone="warning"
          title="The proposed change could not be loaded"
          action={
            <Button size="xs" variant="outline" onClick={onRetry}>
              <RefreshCw className="size-3" aria-hidden="true" />
              Retry
            </Button>
          }
        >
          {diffError}
        </InlineAlert>
      )}

      {diff === null && diffError === null && (
        <p role="status" className="text-sm text-muted-foreground">
          Loading the proposed change…
        </p>
      )}

      {diff !== null && files.length === 0 && (
        <p className="text-sm text-muted-foreground">No files were changed for this investigation.</p>
      )}

      {files.length === 1 && (
        <div className="min-w-0">
          <p className="mb-1 font-mono text-xs font-medium text-foreground">{files[0].path}</p>
          <DiffBody diff={files[0].diff} />
        </div>
      )}

      {files.length > 1 && (
        <Tabs defaultValue={files[0].path}>
          <TabsList aria-label="Changed files">
            {files.map((file) => (
              <TabsTrigger key={file.path} value={file.path} className="font-mono text-xs">
                {file.path}
              </TabsTrigger>
            ))}
          </TabsList>
          {files.map((file) => (
            <TabsContent key={file.path} value={file.path}>
              <DiffBody diff={file.diff} />
            </TabsContent>
          ))}
        </Tabs>
      )}
    </section>
  );
}
