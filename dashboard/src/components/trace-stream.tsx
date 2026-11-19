import {
  Bot,
  CircleCheck,
  FileCode,
  GitCommitHorizontal,
  ScrollText,
  Wrench,
  type LucideIcon,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { LiveLabel } from "@/components/pills";
import { timeOfDay } from "@/lib/format";
import { stepKey } from "@/lib/steps";
import type { Finding, Step, StepKind } from "@/lib/types";
import { cn } from "@/lib/utils";

const stepIcon: Record<StepKind, LucideIcon> = {
  ToolCall: Wrench,
  Finding: ScrollText,
  Progress: Bot,
};

const stepTone: Record<StepKind, string> = {
  ToolCall: "text-info",
  Finding: "text-mint",
  Progress: "text-muted-foreground",
};

function StepRow({ step }: { step: Step }) {
  const Icon = stepIcon[step.kind];

  return (
    <li className="relative pl-8">
      <span className="absolute top-1 left-0 grid size-5 place-items-center rounded-full border border-border bg-card">
        <Icon className={cn("size-3", stepTone[step.kind])} aria-hidden="true" />
      </span>
      <div className="px-1 py-0.5">
        <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span className="font-mono text-xs tracking-[0.1em] text-muted-foreground uppercase">{step.kind}</span>
          {step.tool !== undefined && <span className="font-mono text-xs text-info">{step.tool}</span>}
          <span className="font-mono text-xs text-muted-foreground" title={step.time}>
            {timeOfDay(step.time)}
          </span>
        </span>
        <span className="mt-0.5 block text-sm leading-snug text-foreground">{step.summary}</span>
      </div>
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
  findings?: Finding[];
  className?: string;
}) {
  return (
    <section aria-label="Agent trace" className={cn("flex flex-col gap-2", className)} data-testid="trace">
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

      <ol className="flex max-h-[24rem] flex-col gap-2 overflow-y-auto" aria-live="polite">
        {steps.map((step) => (
          <StepRow key={stepKey(step)} step={step} />
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
            {findings.map((finding, index) => (
              <li
                key={`${String(index)}-${finding.kind}-${finding.source}`}
                className="flex items-start gap-2 border-t border-border-subtle pt-1.5"
              >
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
