import { Plus, RefreshCw } from "lucide-react";
import { NavLink } from "react-router-dom";

import { InlineAlert } from "@/components/inline-alert";
import { StatusPill } from "@/components/status-pill";
import { Button } from "@/components/ui/button";
import { ageLabel } from "@/lib/format";
import { useAuth } from "@/lib/use-auth";
import { cn } from "@/lib/utils";

// Shared by the desktop rail and the mobile sheet. Every row is a real link, so history is reachable by
// Tab and Enter without a custom keyboard model, and the current page is announced through aria-current.
export function HistorySidebar({
  heading = true,
  onNavigate,
}: {
  heading?: boolean;
  onNavigate?: () => void;
}) {
  const { pages, historyStatus, historyError, refreshHistory } = useAuth();
  const loading = historyStatus === "loading";

  return (
    <section aria-label="Investigation history" className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-2 px-3 py-3">
        {heading ? (
          <h2 className="text-sm font-semibold tracking-tight text-foreground">History</h2>
        ) : null}
        <Button
          variant="ghost"
          size="icon-sm"
          className="ml-auto"
          aria-label="Refresh investigation history"
          onClick={() => {
            void refreshHistory();
          }}
        >
          <RefreshCw className={cn("size-3.5", loading && "motion-safe:animate-spin")} aria-hidden="true" />
        </Button>
      </div>

      {historyError !== null && (
        <div className="px-3 pb-3">
          <InlineAlert
            tone="warning"
            title="History could not be refreshed"
            action={
              <Button size="xs" variant="outline" onClick={() => void refreshHistory()}>
                Retry
              </Button>
            }
          >
            {historyError}
          </InlineAlert>
        </div>
      )}

      {loading && pages.length === 0 ? (
        <div className="space-y-2 px-3" role="status" aria-busy="true">
          <span className="sr-only">Loading investigation history</span>
          {[0, 1, 2].map((row) => (
            <div key={row} className="h-14 animate-pulse rounded-md border border-border-subtle bg-muted" />
          ))}
        </div>
      ) : null}

      {!loading && pages.length === 0 && historyError === null ? (
        <div className="px-3 pb-3">
          <div className="rounded-lg border border-dashed border-border px-3 py-5 text-center">
            <p className="text-sm font-medium text-foreground">No investigations yet</p>
            <p className="mt-1 text-xs text-muted-foreground">
              Ask a question and it will appear here for everyone in the installation.
            </p>
            <Button asChild size="sm" variant="outline" className="mt-3">
              <NavLink to="/" onClick={onNavigate}>
                <Plus className="size-3.5" aria-hidden="true" />
                Start one
              </NavLink>
            </Button>
          </div>
        </div>
      ) : null}

      {pages.length > 0 ? (
        <nav aria-label="Past investigations" className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
          <ol className="space-y-1">
            {pages.map((page) => (
              <li key={page.id}>
                <NavLink
                  to={`/pages/${encodeURIComponent(page.id)}`}
                  onClick={onNavigate}
                  data-page-id={page.id}
                  className={({ isActive }) =>
                    cn(
                      "block rounded-md border border-transparent px-2.5 py-2 transition-colors",
                      isActive
                        ? "border-primary/40 bg-primary-soft"
                        : "hover:border-border-subtle hover:bg-accent",
                    )
                  }
                >
                  <span className="line-clamp-2 text-sm leading-snug font-medium text-foreground">
                    {page.title}
                  </span>
                  <span className="mt-1.5 flex flex-wrap items-center gap-1.5">
                    <StatusPill status={page.status} />
                    <span className="font-mono text-xs text-muted-foreground" title={page.updatedAt}>
                      {ageLabel(page.updatedAt)} ago
                    </span>
                  </span>
                </NavLink>
              </li>
            ))}
          </ol>
        </nav>
      ) : null}
    </section>
  );
}
