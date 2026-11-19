import { LogOut, Plus, Settings, SquareTerminal } from "lucide-react";
import { NavLink } from "react-router-dom";

import { BrandLockup } from "@/components/brand";
import { ThemeToggle } from "@/components/theme-toggle";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { Session } from "@/lib/types";
import { cn } from "@/lib/utils";

function navClass({ isActive }: { isActive: boolean }): string {
  return cn(
    "rounded-md px-2 py-1 text-sm font-medium transition-colors",
    isActive ? "bg-primary-soft text-primary-soft-foreground" : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
  );
}

// One header for the root and settings routes. The brand block is one link home, and on the root route
// it also clears every active filter.
export function AppHeader({
  session,
  version,
  onBrandActivate,
  onOpenCli,
  onPageWorkload,
  onSignOut,
}: {
  session: Session;
  version: string;
  onBrandActivate?: () => void;
  onOpenCli: () => void;
  onPageWorkload?: () => void;
  onSignOut: () => void;
}) {
  return (
    <header className="sticky top-0 z-30 border-b border-border bg-background">
      <div className="mx-auto flex w-full max-w-[1400px] flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2 sm:px-5">
        <BrandLockup onActivate={onBrandActivate} />

        <nav aria-label="Primary" className="flex items-center gap-1">
          <NavLink to="/" end className={navClass}>
            Cluster
          </NavLink>
          <NavLink to="/settings" className={navClass}>
            <span className="inline-flex items-center gap-1.5">
              <Settings className="size-3.5" aria-hidden="true" /> Settings
            </span>
          </NavLink>
        </nav>

        <div className="ml-auto flex flex-wrap items-center justify-end gap-1.5">
          <ThemeToggle />

          {session.mode === "disabled" ? (
            <Badge variant="warning">auth disabled</Badge>
          ) : (
            <span
              className="hidden items-center gap-1.5 rounded-md border border-border bg-card px-2 py-1 text-xs text-muted-foreground sm:inline-flex"
              title={`Signed in with a bootstrap-token session; the server reports its expiry as ${session.expiresAt}.`}
            >
              <span className="grid size-4 place-items-center rounded-full bg-primary-soft font-mono text-xs text-primary-soft-foreground">
                {session.username.slice(0, 2)}
              </span>
              {session.username}
            </span>
          )}

          <span className="hidden font-mono text-xs text-muted-foreground md:inline" title="pagerless-server version">
            {version}
          </span>

          <Button variant="ghost" size="sm" onClick={onOpenCli} aria-label="Open the CLI equivalent drawer">
            <SquareTerminal className="size-3.5" aria-hidden="true" />
            <span className="hidden sm:inline">CLI</span>
          </Button>

          {onPageWorkload !== undefined && (
            <Button size="sm" onClick={onPageWorkload}>
              <Plus className="size-3.5" aria-hidden="true" />
              <span className="hidden sm:inline">Page a workload</span>
              <span className="sr-only sm:hidden">Page a workload</span>
            </Button>
          )}

          <Button variant="ghost" size="icon-sm" aria-label="Sign out" onClick={onSignOut}>
            <LogOut className="size-4" aria-hidden="true" />
          </Button>
        </div>
      </div>
    </header>
  );
}
