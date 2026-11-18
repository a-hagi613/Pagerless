import { useState, type ComponentType } from "react";
import {
  Activity,
  Bot,
  ChevronRight,
  Cpu,
  FileCode,
  GitCommitHorizontal,
  ScrollText,
  Terminal,
} from "lucide-react";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { cn } from "@/lib/utils";

import { Badge } from "@/components/ui/badge";

import type { Evidence, EvidenceSource, Page } from "../types";
import { LivePulse } from "./brand";

const sourceIcon: Record<EvidenceSource, ComponentType<{ className?: string }>> = {
  "Kubernetes event": Cpu,
  "Pod log": ScrollText,
  Loki: FileCode,
  Prometheus: Activity,
  "GitHub commit": GitCommitHorizontal,
  Agent: Bot,
};

const sourceTone: Record<EvidenceSource, string> = {
  "Kubernetes event": "text-sky-300",
  "Pod log": "text-slate-300",
  Loki: "text-violet-300",
  Prometheus: "text-amber-300",
  "GitHub commit": "text-mint",
  Agent: "text-primary",
};

function EvidenceItem({ evidence, index, compact }: { evidence: Evidence; index: number; compact: boolean }) {
  const [open, setOpen] = useState(index === 0);
  const Icon = sourceIcon[evidence.source];

  return (
    <li className="relative pl-8">
      <span className="absolute top-1.5 left-[9px] h-full w-px bg-border-subtle last:hidden" aria-hidden="true" />
      <span className="absolute top-1 left-0 grid size-[19px] place-items-center rounded-full border border-border bg-card">
        <Icon className={cn("size-3", sourceTone[evidence.source])} />
      </span>

      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger className="group flex w-full items-start gap-2 rounded-md px-1 py-0.5 text-left transition-colors hover:bg-accent/40">
          <ChevronRight
            className={cn("mt-0.5 size-3 shrink-0 text-muted-foreground transition-transform", open && "rotate-90")}
            aria-hidden="true"
          />
          <span className="min-w-0 flex-1">
            <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
              <span className="font-mono text-[12px] tracking-[0.12em] text-muted-foreground uppercase">
                {evidence.source}
              </span>
              <span className="font-mono text-[12px] text-muted-foreground">{evidence.at}</span>
            </span>
            <span className={cn("mt-0.5 block text-[13px] leading-snug text-foreground", compact && "text-xs")}>
              {evidence.summary}
            </span>
          </span>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <div className="mt-1.5 overflow-hidden rounded-md border border-border-subtle bg-[#0b1324]">
            <div className="flex items-center gap-1.5 border-b border-border-subtle bg-card/50 px-2 py-1">
              <Terminal className="size-3 text-primary" aria-hidden="true" />
              <code className="truncate font-mono text-[12px] text-muted-foreground" title={evidence.tool}>
                {evidence.tool}
              </code>
            </div>
            <pre className="overflow-x-auto px-2 py-1.5 font-mono text-[12px] leading-[1.55] whitespace-pre text-slate-300">
              {evidence.output.join("\n")}
            </pre>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}

export function TraceStream({
  page,
  compact = false,
  className,
}: {
  page: Page;
  compact?: boolean;
  className?: string;
}) {
  const latest = page.evidence[page.evidence.length - 1];
  return (
    <div className={cn("flex flex-col gap-2", className)}>
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">
          investigation trace · {page.evidence.length} findings
        </span>
        {page.status === "investigating" ? (
          <LivePulse label="streaming" />
        ) : (
          <Badge variant={page.proposal === undefined ? "muted" : "mint"}>
            {page.proposal === undefined ? "trace complete" : "trace complete · proposal ready"}
          </Badge>
        )}
      </div>
      <ol className={cn("flex flex-col gap-2.5", compact && "gap-1.5")}>
        {page.evidence.map((evidence, index) => (
          <EvidenceItem key={evidence.id} evidence={evidence} index={index} compact={compact} />
        ))}
        {page.status === "investigating" && (
          <li className="relative pl-8">
            <span className="absolute top-1 left-0 grid size-[19px] place-items-center rounded-full border border-warning/40 bg-warning/10">
              <span className="size-1.5 rounded-full bg-warning" aria-hidden="true" />
            </span>
            <p className="py-0.5 font-mono text-[12px] text-warning">
              awaiting next finding{latest === undefined ? "" : " · tool call in flight"}…
            </p>
          </li>
        )}
      </ol>
    </div>
  );
}
