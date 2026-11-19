import {
  Bot,
  ChevronRight,
  CircleCheck,
  FileCode,
  GitCommitHorizontal,
  ScrollText,
  Terminal,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { LiveLabel } from "@/components/pills";
import type { Step, StepKind } from "@/lib/types";
import { cn } from "@/lib/utils";

const stepIcon: Record<StepKind, LucideIcon> = {
  tool: Wrench,
  finding: ScrollText,
  progress: Bot,
  phase: CircleCheck,
};

const stepTone: Record<StepKind, string> = {
  tool: "text-info",
  finding: "text-mint",
  progress: "text-muted-foreground",
  phase: "text-primary",
};

function StepRow({ step, expanded }: { step: Step; expanded: boolean }) {
  const [open, setOpen] = useState(expanded);
  const Icon = stepIcon[step.kind];
  const time = (() => {
    const parsed = Date.parse(step.at);
    return Number.isNaN(parsed) ? step.at : new Date(parsed).toLocaleTimeString();
  })();

  const body = (
    <span className="min-w-0 flex-1">
      <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        <span className="font-mono text-xs tracking-[0.1em] text-muted-foreground uppercase">{step.kind}</span>
        {step.tool !== undefined && <span className="font-mono text-xs text-info">{step.tool}</span>}
        <span className="font-mono text-xs text-muted-foreground">{time}</span>
        {step.source !== undefined && <span className="text-xs text-muted-foreground">{step.source}</span>}
      </span>
      <span className="mt-0.5 block text-sm leading-snug text-foreground">{step.summary}</span>
    </span>
  );

  return (
    <li className="relative pl-8">
      <span className="absolute top-1 left-0 grid size-5 place-items-center rounded-full border border-border bg-card">
        <Icon className={cn("size-3", stepTone[step.kind])} aria-hidden="true" />
      </span>
      {step.detail === undefined ? (
        <div className="flex items-start gap-2 px-1 py-0.5">{body}</div>
      ) : (
        <Collapsible open={open} onOpenChange={setOpen}>
          <CollapsibleTrigger className="group flex w-full items-start gap-2 rounded-md px-1 py-0.5 text-left transition-colors hover:bg-accent">
            <ChevronRight
              className={cn("mt-1 size-3 shrink-0 text-muted-foreground transition-transform", open && "rotate-90")}
              aria-hidden="true"
            />
            {body}
          </CollapsibleTrigger>
          <CollapsibleContent>
            <div className="mt-1 overflow-hidden rounded-md border border-border-subtle bg-terminal">
              <div className="flex items-center gap-1.5 border-b border-white/10 px-2 py-1">
                <Terminal className="size-3 text-terminal-muted" aria-hidden="true" />
                <code className="truncate font-mono text-xs text-terminal-muted">{step.tool ?? "output"}</code>
              </div>
              <pre className="max-h-64 overflow-auto px-2 py-1.5 font-mono text-xs leading-relaxed whitespace-pre text-terminal-foreground">
                {step.detail}
              </pre>
            </div>
          </CollapsibleContent>
        </Collapsible>
      )}
    </li>
  );
}

// The trace grows as the server sends events. The live affordance is the arriving line, not a pulsing
// dot: a static label states the stream is open, and the count moves on its own.
export function TraceStream({
  steps,
  streaming,
  findings,
  className,
}: {
  steps: Step[];
  streaming: boolean;
  findings?: { id: string; kind: string; source: string; summary: string }[];
  className?: string;
}) {
  return (
    <section aria-label="Agent trace" className={cn("flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 className="font-mono text-sm tracking-[0.12em] text-muted-foreground uppercase">
          Investigation trace · {steps.length} steps
        </h4>
        {streaming ? (
          <LiveLabel label="streaming" />
        ) : (
          <Badge variant="muted">
            <CircleCheck className="size-3" aria-hidden="true" /> stream closed
          </Badge>
        )}
      </div>

      {steps.length === 0 && !streaming && <p className="text-sm text-muted-foreground">No steps recorded yet.</p>}

      <ol className="flex flex-col gap-2">
        {steps.map((step, index) => (
          <StepRow key={step.id} step={step} expanded={index === 0} />
        ))}
        {streaming && (
          <li className="relative pl-8">
            <span className="absolute top-1 left-0 grid size-5 place-items-center rounded-full border border-border bg-card">
              <GitCommitHorizontal className="size-3 text-muted-foreground" aria-hidden="true" />
            </span>
            <p className="py-0.5 font-mono text-xs text-muted-foreground">awaiting the next step…</p>
          </li>
        )}
      </ol>

      {findings !== undefined && findings.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <h4 className="font-mono text-sm tracking-[0.12em] text-muted-foreground uppercase">
            Findings · {findings.length}
          </h4>
          <ul className="flex flex-col gap-1.5">
            {findings.map((finding) => (
              <li key={finding.id} className="flex items-start gap-2 border-t border-border-subtle pt-1.5">
                <FileCode className="mt-0.5 size-3.5 shrink-0 text-mint" aria-hidden="true" />
                <span className="min-w-0">
                  <span className="font-mono text-xs tracking-[0.1em] text-muted-foreground uppercase">
                    {finding.kind} · {finding.source}
                  </span>
                  <span className="mt-0.5 block text-sm leading-snug text-foreground">{finding.summary}</span>
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}
