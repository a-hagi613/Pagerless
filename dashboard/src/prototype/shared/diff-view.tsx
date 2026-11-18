import { useState } from "react";
import { Check, CircleCheck, Copy, FileDiff, ShieldCheck } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

import { shortHash } from "../format";
import type { Proposal } from "../types";

export function DiffView({
  proposal,
  className,
  showChecks = true,
}: {
  proposal: Proposal;
  className?: string;
  showChecks?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  // Number the code lines only; the "meta" row is a file header, not a line.
  const rows = proposal.lines.map((line, index) => ({
    line,
    number:
      line.kind === "meta"
        ? undefined
        : proposal.lines.slice(0, index + 1).filter((candidate) => candidate.kind !== "meta").length,
  }));

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(proposal.hash);
      setCopied(true);
      window.setTimeout(() => {
        setCopied(false);
      }, 1_600);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className={cn("overflow-hidden rounded-lg border border-border-subtle bg-[#0b1324]", className)}>
      <div className="flex flex-wrap items-center gap-2 border-b border-border-subtle bg-card/60 px-3 py-2">
        <FileDiff className="size-3.5 text-mint" aria-hidden="true" />
        <span className="font-mono text-[12px] text-foreground">{proposal.path}</span>
        <Badge variant="muted" className="ml-auto">
          {proposal.action}
        </Badge>
        <Badge variant="mint">
          <ShieldCheck className="size-3" aria-hidden="true" /> verified
        </Badge>
      </div>

      <div className="overflow-x-auto">
        <table className="w-full border-collapse font-mono text-[12px] leading-5">
          <caption className="sr-only">Proposed change to {proposal.path}</caption>
          <tbody>
            {rows.map(({ line, number }) => {
              const marker = line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " ";
              return (
                <tr
                  key={`${String(number ?? "meta")}-${line.text}`}
                  className={cn(
                    "align-top",
                    line.kind === "add" && "bg-primary/10",
                    line.kind === "remove" && "bg-destructive/10",
                    line.kind === "meta" && "bg-accent/60",
                  )}
                >
                  <td className="w-9 border-r border-border-subtle/60 pr-2 text-right text-[12px] text-muted-foreground select-none">
                    {number ?? ""}
                  </td>
                  <td
                    className={cn(
                      "w-4 pl-2 select-none",
                      line.kind === "add" && "text-primary",
                      line.kind === "remove" && "text-destructive",
                      line.kind === "meta" && "text-mint",
                    )}
                  >
                    {marker}
                  </td>
                  <td
                    className={cn(
                      "pr-3 whitespace-pre",
                      line.kind === "add" && "text-mint",
                      line.kind === "remove" && "text-destructive/90",
                      line.kind === "context" && "text-slate-400",
                      line.kind === "meta" && "text-muted-foreground",
                    )}
                  >
                    {line.text}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border-subtle px-3 py-2">
        <button
          type="button"
          onClick={() => void copy()}
          className="inline-flex items-center gap-1.5 rounded border border-border-subtle bg-muted/50 px-1.5 py-0.5 font-mono text-[12px] text-muted-foreground transition-colors hover:text-foreground"
          title={proposal.hash}
        >
          {copied ? <Check className="size-3 text-primary" aria-hidden="true" /> : <Copy className="size-3" aria-hidden="true" />}
          sha256:{shortHash(proposal.hash)}
        </button>
        <span className="font-mono text-[12px] text-muted-foreground">base {proposal.baseCommit}</span>
        <span className="font-mono text-[12px] text-muted-foreground">branch {proposal.branch}</span>
      </div>

      {showChecks && (
        <ul className="flex flex-col gap-1 border-t border-border-subtle bg-card/40 px-3 py-2">
          {proposal.checks.map((check) => (
            <li key={check} className="flex items-start gap-1.5 text-[12px] text-muted-foreground">
              <CircleCheck className="mt-0.5 size-3 shrink-0 text-primary" aria-hidden="true" />
              {check}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
