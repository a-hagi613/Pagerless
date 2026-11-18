import { cn } from "@/lib/utils";

export function PagerMark({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "relative grid size-9 shrink-0 place-items-center overflow-hidden rounded-lg border border-border bg-[#0b1324]",
        className,
      )}
    >
      <img
        src="/pagerless-mascot.png"
        alt=""
        width={256}
        height={256}
        className="size-full scale-[1.12] object-contain"
      />
    </span>
  );
}

export function PromptChip({ className, children = ">_" }: { className?: string; children?: string }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded border border-primary/35 bg-primary/10 px-1.5 font-mono text-[12px] font-semibold tracking-tight text-primary",
        className,
      )}
    >
      {children}
    </span>
  );
}

export function LivePulse({ label = "live", className }: { label?: string; className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-1.5 font-mono text-[12px] tracking-[0.18em] text-muted-foreground uppercase", className)}>
      <span className="size-1.5 rounded-full bg-primary" aria-hidden="true" />
      {label}
    </span>
  );
}

export function Wordmark({ compact = false }: { compact?: boolean }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      <PagerMark className={compact ? "size-7" : undefined} />
      <span className="flex min-w-0 flex-col leading-none">
        <span className="truncate text-[15px] font-semibold tracking-tight text-foreground">
          Pagerless
        </span>
        {!compact && (
          <span className="mt-1 font-mono text-[12px] tracking-[0.14em] text-muted-foreground uppercase">
            incident → PR
          </span>
        )}
      </span>
    </span>
  );
}
