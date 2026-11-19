import { Check, Copy, FileDiff, Fingerprint } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { diffCounts, parseUnifiedDiff } from "@/lib/diff";
import { shortHash, timestamp } from "@/lib/format";
import type { Proposal } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useCopy } from "@/lib/use-copy";

const lineTone = {
  add: "bg-diff-add-surface text-diff-add-foreground",
  remove: "bg-diff-remove-surface text-diff-remove-foreground",
  context: "text-foreground",
  meta: "bg-muted text-muted-foreground",
} as const;

const marker = { add: "+", remove: "−", context: " ", meta: "" } as const;

const markerLabel = { add: "Added line", remove: "Removed line", context: "Context line", meta: "File header" } as const;

// The diff is the stored Proposal: the server rendered it, the Portal only reads it. There is no client-side
// diffing, so what the approver reviews is exactly what the hash binds.
export function DiffView({ proposal, className }: { proposal: Proposal; className?: string }) {
  const { copied, copy } = useCopy();
  const change = proposal.renderedChange;
  const patch = change?.patch ?? "";
  const lines = parseUnifiedDiff(patch);
  const counts = diffCounts(lines);
  const path = change?.path ?? "";

  return (
    <section aria-label="Proposed change" className={cn("flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <FileDiff className="size-4 text-mint" aria-hidden="true" />
        <h4 className="text-sm font-semibold text-foreground">Proposed change</h4>
        <Badge variant="muted">{proposal.action}</Badge>
        <span className="ml-auto font-mono text-xs text-muted-foreground">
          <span className="text-diff-add-foreground">+{counts.added}</span>{" "}
          <span className="text-diff-remove-foreground">−{counts.removed}</span>
        </span>
      </div>

      {patch === "" ? (
        <p className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-sm text-muted-foreground">
          This Proposal makes no file change; the Diagnosis is the deliverable.
        </p>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border-subtle">
          {path !== "" && (
            <div className="flex flex-wrap items-center gap-2 border-b border-border-subtle bg-muted px-3 py-1.5">
              <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground">{path}</span>
              <span className="font-mono text-xs text-muted-foreground">attempt {proposal.attempt}</span>
            </div>
          )}
          <div className="max-h-[26rem] overflow-auto">
            <table className="w-full border-collapse font-mono text-xs leading-5">
              <caption className="sr-only">Proposed change to {path === "" ? "the Target Repository" : path}</caption>
              <tbody>
                {lines.map((line, index) => (
                  <tr key={`${String(index)}-${line.text}`} className={cn("align-top", lineTone[line.kind])}>
                    <td className="w-8 border-r border-border-subtle pr-1 text-right text-muted-foreground select-none">
                      {line.before ?? ""}
                    </td>
                    <td className="w-8 border-r border-border-subtle pr-1 text-right text-muted-foreground select-none">
                      {line.after ?? ""}
                    </td>
                    <td className="w-5 pl-1.5 font-semibold select-none">
                      <span aria-hidden="true">{marker[line.kind]}</span>
                      <span className="sr-only">{markerLabel[line.kind]}</span>
                    </td>
                    <td className="pr-3 whitespace-pre">{line.text}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Button
          variant="outline"
          size="xs"
          aria-label={`Copy the Proposal hash ${proposal.hash}`}
          onClick={() => void copy(proposal.hash)}
        >
          {copied ? <Check className="size-3" aria-hidden="true" /> : <Copy className="size-3" aria-hidden="true" />}
          <span className="font-mono">sha256:{shortHash(proposal.hash)}</span>
          <span className="sr-only">{copied ? "Copied" : ""}</span>
        </Button>
        {proposal.base?.commit !== undefined && proposal.base.commit !== "" && (
          <span className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
            <Fingerprint className="size-3" aria-hidden="true" /> base {shortHash(proposal.base.commit)}
          </span>
        )}
        <span className="text-xs text-muted-foreground">expires {timestamp(proposal.expiresAt)}</span>
      </div>

      {proposal.parameters !== undefined && Object.keys(proposal.parameters).length > 0 && (
        <details className="rounded-lg border border-border-subtle bg-panel px-3 py-2">
          <summary className="cursor-pointer text-xs font-medium text-muted-foreground">Parameters</summary>
          <pre className="mt-2 overflow-x-auto font-mono text-xs leading-relaxed text-foreground">
            {JSON.stringify(proposal.parameters, null, 2)}
          </pre>
        </details>
      )}
    </section>
  );
}
