import { useCallback, useEffect } from "react";
import { ArrowLeft, ArrowRight } from "lucide-react";

import { cn } from "@/lib/utils";

import { PROTOTYPE_VARIANTS, type VariantKey } from "./variants";

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  if (target.isContentEditable) {
    return true;
  }
  const tag = target.tagName.toLowerCase();
  return tag === "input" || tag === "textarea" || tag === "select";
}

// The floating variant switcher. It is intentionally styled as chrome, not as
// part of any variant, and it writes ?variant= so a variant stays shareable.
export function PrototypeSwitcher({
  current,
  onSelect,
  lift = false,
  hidden = false,
}: {
  current: VariantKey;
  onSelect: (key: VariantKey) => void;
  lift?: boolean;
  hidden?: boolean;
}) {
  const index = PROTOTYPE_VARIANTS.findIndex((variant) => variant.key === current);

  const cycle = useCallback(
    (delta: number) => {
      const next = PROTOTYPE_VARIANTS[(index + delta + PROTOTYPE_VARIANTS.length) % PROTOTYPE_VARIANTS.length];
      if (next !== undefined) {
        onSelect(next.key);
      }
    },
    [index, onSelect],
  );

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (hidden || event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) {
        return;
      }
      if (isTypingTarget(event.target)) {
        return;
      }
      if (event.key === "ArrowLeft") {
        event.preventDefault();
        cycle(-1);
      }
      if (event.key === "ArrowRight") {
        event.preventDefault();
        cycle(1);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [cycle, hidden]);

  if (hidden) {
    return null;
  }

  return (
    <div
      className={cn(
        "pointer-events-none fixed inset-x-0 z-[60] flex justify-center px-2",
        lift ? "bottom-[112px] sm:bottom-3" : "bottom-3",
      )}
    >
      <div
        className="pointer-events-auto flex max-w-full items-center gap-1 rounded-full border border-border bg-[#0b1324]/85 p-1 shadow-[0_12px_40px_-12px_rgba(0,0,0,0.9)] backdrop-blur"
        role="group"
        aria-label="Prototype variant switcher"
      >
        <span
          className="hidden rounded-full bg-mint-surface px-2 py-1 font-mono text-[12px] tracking-[0.16em] text-mint uppercase sm:inline"
          title="Throwaway Wayfinder prototype for ticket 83: in-memory data, no API, no auth. Not production UI."
        >
          prototype · 83
        </span>
        <button
          type="button"
          onClick={() => {
            cycle(-1);
          }}
          aria-label="Previous variant"
          className="grid size-7 shrink-0 place-items-center rounded-full text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <ArrowLeft className="size-3.5" />
        </button>

        <div className="flex items-center gap-0.5">
          {PROTOTYPE_VARIANTS.map((variant, position) => {
            const active = variant.key === current;
            return (
              <button
                key={variant.key}
                type="button"
                onClick={() => {
                  onSelect(variant.key);
                }}
                aria-pressed={active}
                title={variant.name}
                className={cn(
                  "flex items-center gap-1.5 rounded-full px-2 py-1 font-mono text-[12px] transition-colors",
                  active ? "bg-primary/15 text-primary" : "text-muted-foreground hover:text-foreground",
                )}
              >
                <span className={cn("font-semibold", !active && "text-muted-foreground")}>{variant.key}</span>
                <span className={cn("hidden sm:inline", active ? "text-primary" : "text-muted-foreground")}>
                  {variant.name}
                </span>
                <span className="sr-only">
                  {position === index ? "current variant" : `switch to ${variant.name}`}
                </span>
              </button>
            );
          })}
        </div>

        <button
          type="button"
          onClick={() => {
            cycle(1);
          }}
          aria-label="Next variant"
          className="grid size-7 shrink-0 place-items-center rounded-full text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <ArrowRight className="size-3.5" />
        </button>
      </div>
    </div>
  );
}
