import { Check, CircleAlert, CircleCheck, Copy, FileDiff, ShieldCheck } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { parseUnifiedDiff } from "@/lib/diff";
import { shortHash } from "@/lib/format";
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

export function DiffView({ proposal, className }: { proposal: Proposal; className?: string }) {
  const { copied, copy } = useCopy();
  const passed = proposal.validation.checks.every((check) => check.passed);

  return (
    <section aria-label="Proposed change" className={cn("flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <FileDiff className="size-4 text-mint" aria-hidden="true" />
        <h4 className="text-sm font-semibold text-foreground">Proposed change</h4>
        <Badge variant="muted" className="ml-auto">
          {proposal.action === "FilePatch" ? "File patch" : "Diagnosis only"}
        </Badge>
        <Badge variant={passed ? "mint" : "warning"}>
          <ShieldCheck className="size-3" aria-hidden="true" />
          {passed ? "Validated" : "Needs review"}
        </Badge>
      </div>

      {proposal.files.map((file) => {
        const lines = parseUnifiedDiff(file.diff);
        return (
          <div key={file.path} className="overflow-hidden border-t border-border-subtle">
            <div className="flex flex-wrap items-center gap-2 border-b border-border-subtle bg-muted px-3 py-1.5">
              <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground">{file.path}</span>
              <span className="font-mono text-xs text-muted-foreground">{proposal.branch}</span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse font-mono text-xs leading-5">
                <caption className="sr-only">Proposed change to {file.path}</caption>
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
        );
      })}

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Button
          variant="outline"
          size="xs"
          aria-label={`Copy proposal hash ${proposal.hash}`}
          onClick={() => void copy(proposal.hash)}
        >
          {copied ? <Check className="size-3" aria-hidden="true" /> : <Copy className="size-3" aria-hidden="true" />}
          <span className="font-mono">sha256:{shortHash(proposal.hash)}</span>
          <span className="sr-only">{copied ? "Copied" : ""}</span>
        </Button>
        <span className="font-mono text-xs text-muted-foreground">base {shortHash(proposal.baseCommit)}</span>
        <span className="text-xs text-muted-foreground">{proposal.summary}</span>
      </div>

      {proposal.warnings.length > 0 && (
        <ul className="flex flex-col gap-1">
          {proposal.warnings.map((warning) => (
            <li key={warning} className="flex items-start gap-1.5 text-xs text-warning">
              <CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
              {warning}
            </li>
          ))}
        </ul>
      )}

      {proposal.validation.checks.length > 0 && (
        <ul className="flex flex-col gap-1 border-t border-border-subtle pt-2">
          {proposal.validation.checks.map((check) => (
            <li key={check.name} className="flex items-start gap-1.5 text-xs">
              {check.passed ? (
                <CircleCheck className="mt-0.5 size-3 shrink-0 text-primary" aria-hidden="true" />
              ) : (
                <CircleAlert className="mt-0.5 size-3 shrink-0 text-destructive" aria-hidden="true" />
              )}
              <span className="min-w-0">
                <span className="font-medium text-foreground">{check.passed ? "Passed" : "Failed"}:</span>{" "}
                <span className="text-muted-foreground">
                  {check.name}
                  {check.detail === undefined ? "" : ` — ${check.detail}`}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}

      <p className="text-xs text-muted-foreground">{proposal.validation.admission}</p>
    </section>
  );
}
