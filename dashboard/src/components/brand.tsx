import { Link } from "react-router-dom";

import { cn } from "@/lib/utils";

// The retro pager mark, drawn at 40 px and shared by the header, the sign-in card, and the docs.
export function PagerMark({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "grid size-10 shrink-0 place-items-center overflow-hidden rounded-lg border border-border bg-terminal",
        className,
      )}
    >
      <img src="/pagerless-mascot.png" alt="" width={256} height={256} className="size-full scale-[1.12] object-contain" />
    </span>
  );
}

export function PromptChip({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded border border-primary/50 bg-primary-soft px-1.5 font-mono text-xs font-semibold tracking-tight text-primary-soft-foreground",
        className,
      )}
    >
      {">_"}
    </span>
  );
}

// One target: the mark, the wordmark, the >_ chip, and the subtitle. Activating it starts a new
// investigation, which is the "start over" control the product promises.
export function BrandLockup({ onActivate, className }: { onActivate?: () => void; className?: string }) {
  return (
    <Link
      to="/"
      onClick={onActivate}
      aria-label="Pagerless investigations — start a new investigation"
      className={cn(
        "flex min-w-0 items-center gap-2.5 rounded-lg py-1 pr-1 transition-colors hover:bg-accent",
        className,
      )}
    >
      <PagerMark />
      <span className="flex min-w-0 flex-col leading-none">
        <span className="flex items-center gap-1.5">
          <span className="truncate text-base font-semibold tracking-tight text-foreground">Pagerless</span>
          <PromptChip />
        </span>
        <span className="mt-1 font-mono text-xs tracking-[0.14em] text-muted-foreground uppercase">
          investigations
        </span>
      </span>
    </Link>
  );
}
