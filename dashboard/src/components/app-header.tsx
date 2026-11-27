import type { RefObject } from "react";
import { Menu, Plus } from "lucide-react";
import { Link } from "react-router-dom";

import { BrandLockup } from "@/components/brand";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";

// The one header for chat and history. The mark starts a new investigation; the mobile menu opens the
// same history the desktop sidebar shows. On a narrow viewport the theme control moves into the history
// sheet so the brand, menu and new-investigation control keep their room.
export function AppHeader({
  onOpenHistory,
  historyTrigger,
}: {
  onOpenHistory: () => void;
  historyTrigger?: RefObject<HTMLButtonElement | null>;
}) {
  return (
    <header className="sticky top-0 z-30 border-b border-border bg-background lg:h-14">
      <div className="mx-auto flex w-full max-w-[1400px] items-center gap-2 px-3 py-2 sm:px-5">
        <Button
          ref={historyTrigger}
          variant="ghost"
          size="icon-sm"
          className="lg:hidden"
          aria-label="Open investigation history"
          onClick={onOpenHistory}
        >
          <Menu className="size-4" aria-hidden="true" />
        </Button>

        <BrandLockup />

        <div className="ml-auto flex items-center gap-1.5">
          <ThemeToggle className="hidden sm:inline-flex" />
          <Button asChild size="sm">
            <Link to="/">
              <Plus className="size-3.5" aria-hidden="true" />
              <span className="hidden sm:inline">New investigation</span>
              <span className="sr-only sm:hidden">New investigation</span>
            </Link>
          </Button>
        </div>
      </div>
    </header>
  );
}
