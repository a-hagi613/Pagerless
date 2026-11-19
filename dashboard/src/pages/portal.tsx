import { CircleCheck, GitPullRequest, Radar, RefreshCw, X } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { AppHeader } from "@/components/app-header";
import { CliDrawer } from "@/components/cli-drawer";
import { HealthMatrix } from "@/components/health-matrix";
import { Notice } from "@/components/notices";
import { PageCard } from "@/components/page-card";
import { PageResourceDialog } from "@/components/page-resource-dialog";
import { LiveLabel } from "@/components/pills";
import { Button } from "@/components/ui/button";
import { decidePage, getPage, getPages, subscribeTrace, UnauthenticatedError } from "@/lib/api";
import { signOut } from "@/lib/sign-out";
import {
  NO_FILTERS,
  filterPages,
  isFiltered,
  scopeLabel,
  toggleNamespace,
  toggleNode,
  toggleSeverity,
} from "@/lib/filters";
import { pageTitle } from "@/lib/format";
import type { ClusterMatrix, PageDetail, PageSummary, Severity, Step } from "@/lib/types";
import { useSession } from "@/lib/use-session";
import { cn } from "@/lib/utils";

const QUICKSTART_KEY = "pagerless.quickstart.dismissed";

const severityOptions: { value: Severity; label: string }[] = [
  { value: "critical", label: "Critical" },
  { value: "warning", label: "Warning" },
  { value: "info", label: "Info" },
];

export function PortalPage() {
  const { session, refresh: refreshSession } = useSession();
  const [pages, setPages] = useState<PageSummary[]>([]);
  const [matrix, setMatrix] = useState<ClusterMatrix | null>(null);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState<string | null>(null);

  const [filters, setFilters] = useState(NO_FILTERS);
  // ?page= makes an expanded Page a shareable, refreshable link.
  const [expanded, setExpanded] = useState<string | null>(
    () => new URLSearchParams(window.location.search).get("page"),
  );
  const [details, setDetails] = useState<Record<string, PageDetail>>({});
  const [steps, setSteps] = useState<Record<string, Step[]>>({});
  const [streaming, setStreaming] = useState<string | null>(null);
  const [detailLoading, setDetailLoading] = useState<string | null>(null);
  const [decisionBusy, setDecisionBusy] = useState<string | null>(null);
  const [decisionErrors, setDecisionErrors] = useState<Record<string, string>>({});

  const [cliOpen, setCliOpen] = useState(false);
  const [pageDialogOpen, setPageDialogOpen] = useState(false);
  const [quickstartOpen, setQuickstartOpen] = useState(() => {
    try {
      return window.localStorage.getItem(QUICKSTART_KEY) !== "1";
    } catch {
      return true;
    }
  });

  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const onUnauthenticated = useCallback(
    (cause: unknown): boolean => {
      if (cause instanceof UnauthenticatedError) {
        void refreshSession();
        return true;
      }
      return false;
    },
    [refreshSession],
  );

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const response = await getPages(signal);
        if (!mounted.current) {
          return;
        }
        setPages(response.pages);
        setMatrix(response.matrix);
        setListError(null);
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        if (onUnauthenticated(cause)) {
          return;
        }
        if (mounted.current) {
          setListError(cause instanceof Error ? cause.message : String(cause));
        }
      } finally {
        if (mounted.current) {
          setLoading(false);
        }
      }
    },
    [onUnauthenticated],
  );

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") {
        void load();
      }
    }, 20_000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
  }, [load]);

  // One live trace per expanded card. The detail and the stream are fetched together so a step that
  // arrives before the first render is not lost.
  useEffect(() => {
    if (expanded === null) {
      return;
    }
    const name = expanded;
    setDetailLoading(name);
    setDecisionErrors((current) => ({ ...current, [name]: "" }));

    const mergeDetail = (detail: PageDetail) => {
      if (!mounted.current) {
        return;
      }
      setDetails((current) => ({ ...current, [name]: detail }));
      setSteps((current) => {
        const live = current[name] ?? [];
        const seen = new Set(live.map((step) => step.id));
        const seeded = detail.steps.filter((step) => !seen.has(step.id));
        return { ...current, [name]: [...seeded, ...live] };
      });
      setDetailLoading(null);
    };

    const refetchDetail = () => {
      getPage(name).then(mergeDetail, (cause: unknown) => {
        if (mounted.current && !onUnauthenticated(cause)) {
          setDecisionErrors((current) => ({
            ...current,
            [name]: cause instanceof Error ? cause.message : String(cause),
          }));
        }
      });
    };

    const appendStep = (step: Step) => {
      setSteps((current) => {
        const existing = current[name] ?? [];
        if (existing.some((candidate) => candidate.id === step.id)) {
          return current;
        }
        return { ...current, [name]: [...existing, step] };
      });
    };

    const closeTrace = subscribeTrace(name, {
      onStep: appendStep,
      onPhase: (update) => {
        // The event carries the phase; the Page endpoint is authoritative for the Proposal and the
        // Findings that arrive with it, so the detail is re-read rather than reconstructed.
        refetchDetail();
        setPages((current) =>
          current.map((page) =>
            page.name === name
              ? {
                  ...page,
                  phase: update.phase,
                  pullRequest: update.pullRequest ?? page.pullRequest,
                }
              : page,
          ),
        );
        setDetails((current) => {
          const detail = current[name];
          if (detail === undefined) {
            return current;
          }
          return {
            ...current,
            [name]: { ...detail, phase: update.phase, pullRequest: update.pullRequest ?? detail.pullRequest },
          };
        });
      },
      onDone: () => {
        setStreaming((current) => (current === name ? null : current));
      },
    });
    setStreaming(name);

    refetchDetail();

    return () => {
      closeTrace();
      setStreaming((current) => (current === name ? null : current));
      setDetailLoading((current) => (current === name ? null : current));
    };
  }, [expanded, onUnauthenticated]);

  const resetView = useCallback(() => {
    setFilters(NO_FILTERS);
    setExpanded(null);
    if (window.location.search !== "") {
      window.history.replaceState({}, "", window.location.pathname);
    }
  }, []);

  const decide = useCallback(
    async (page: PageSummary, decision: "approve" | "dismiss", proposalHash: string, reason: string) => {
      setDecisionBusy(page.name);
      setDecisionErrors((current) => ({ ...current, [page.name]: "" }));
      try {
        const updated = await decidePage(page.name, { decision, proposalHash, reason });
        setDetails((current) => ({ ...current, [page.name]: updated }));
        setPages((current) =>
          current.map((candidate) =>
            candidate.name === updated.name
              ? {
                  ...candidate,
                  phase: updated.phase,
                  pullRequest: updated.pullRequest,
                }
              : candidate,
          ),
        );
        await load();
      } catch (cause) {
        if (onUnauthenticated(cause)) {
          return;
        }
        setDecisionErrors((current) => ({
          ...current,
          [page.name]: cause instanceof Error ? cause.message : String(cause),
        }));
      } finally {
        setDecisionBusy(null);
      }
    },
    [load, onUnauthenticated],
  );

  const visible = useMemo(() => filterPages(pages, filters), [pages, filters]);
  const counts = useMemo(() => {
    const base: Record<Severity, number> = { critical: 0, warning: 0, info: 0 };
    for (const page of pages) {
      base[page.severity] += 1;
    }
    return base;
  }, [pages]);

  const proposed = pages.filter((page) => page.phase === "Proposed");
  const expandedPage = pages.find((page) => page.name === expanded) ?? null;

  const dismissQuickstart = () => {
    setQuickstartOpen(false);
    try {
      window.localStorage.setItem(QUICKSTART_KEY, "1");
    } catch {
      // A blocked storage only loses the dismissal.
    }
  };

  if (session === null) {
    return null;
  }

  return (
    <div className="min-h-svh bg-background pb-10">
      <a
        href="#content"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:text-foreground"
      >
        Skip to content
      </a>

      <AppHeader
        session={session}
        onBrandActivate={resetView}
        onOpenCli={() => {
          setCliOpen(true);
        }}
        onPageWorkload={() => {
          setPageDialogOpen(true);
        }}
        onSignOut={() => {
          void signOut(refreshSession);
        }}
      />

      {session.mode === "disabled" && (
        <div className="border-b border-warning/40 bg-warning-surface">
          <p className="mx-auto w-full max-w-[1400px] px-3 py-2 text-sm text-foreground sm:px-5">
            <span className="font-semibold">Authentication is disabled.</span> Anyone who can reach this port
            can approve a pull request; put an authenticating proxy in front of it before exposing it.
          </p>
        </div>
      )}

      <main id="content" className="mx-auto flex w-full max-w-[1400px] flex-col gap-4 px-3 py-4 sm:px-5">
        {listError !== null && (
          <Notice tone="error">
            The Page list could not be read: {listError}.{" "}
            <button
              type="button"
              className="underline underline-offset-2"
              onClick={() => {
                void load();
              }}
            >
              Try again
            </button>
          </Notice>
        )}

        {quickstartOpen && (
          <section
            aria-label="Quickstart"
            className="relative flex flex-wrap items-center gap-x-4 gap-y-2 rounded-xl border border-primary/40 bg-primary-soft px-3 py-2.5 pr-10 sm:px-4"
          >
            <span className="flex items-center gap-1.5 font-mono text-xs tracking-[0.14em] text-primary-soft-foreground uppercase">
              <Radar className="size-3.5" aria-hidden="true" /> quickstart
            </span>
            <span className="flex items-center gap-1.5 text-sm text-foreground">
              <CircleCheck className="size-4 text-primary" aria-hidden="true" />
              <span className="font-medium">1. Cluster connected</span>
              {matrix !== null && (
                <span className="font-mono text-xs text-muted-foreground">
                  {matrix.context} · {matrix.version}
                </span>
              )}
            </span>
            <span className="flex flex-wrap items-center gap-1.5 text-sm text-foreground">
              <GitPullRequest className="size-4 text-muted-foreground" aria-hidden="true" />
              <span className="font-medium">2. Review a Proposal</span>
              <span className="text-muted-foreground">{proposed.length} ready</span>
              <Button
                size="xs"
                variant="secondary"
                disabled={proposed.length === 0}
                onClick={() => {
                  const first = proposed[0];
                  if (first !== undefined) {
                    setFilters(NO_FILTERS);
                    setExpanded(first.name);
                    window.setTimeout(() => {
                      document.getElementById(`page-${first.name}`)?.scrollIntoView({ block: "center" });
                    }, 40);
                  }
                }}
              >
                Open the first Proposal
              </Button>
            </span>
            <button
              type="button"
              onClick={dismissQuickstart}
              aria-label="Dismiss the quickstart"
              className="absolute top-2 right-2 rounded p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
            >
              <X className="size-4" aria-hidden="true" />
            </button>
          </section>
        )}

        {matrix !== null && (
          <HealthMatrix
            matrix={matrix}
            filters={filters}
            onToggleNamespace={(namespace) => {
              setFilters((current) => toggleNamespace(current, namespace));
            }}
            onToggleNode={(node) => {
              setFilters((current) => toggleNode(current, node));
            }}
          />
        )}

        <section aria-labelledby="inbox-heading" className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-2">
            <h2 id="inbox-heading" className="text-base font-semibold tracking-tight text-balance text-foreground">
              Inbox
            </h2>
            <span className="text-sm text-muted-foreground">
              {visible.length} of {pages.length} Pages · {scopeLabel(filters)}
            </span>
            <div role="group" aria-label="Filter by severity" className="flex flex-wrap items-center gap-1">
              {severityOptions.map((option) => {
                const active = filters.severity === option.value;
                return (
                  <button
                    key={option.value}
                    type="button"
                    aria-pressed={active}
                    onClick={() => {
                      setFilters((current) => toggleSeverity(current, option.value));
                    }}
                    className={cn(
                      "inline-flex items-center gap-1.5 rounded-md border px-2 py-1 font-mono text-xs transition-colors",
                      active
                        ? "border-primary bg-primary-soft text-primary-soft-foreground"
                        : "border-border bg-card text-muted-foreground hover:bg-accent hover:text-accent-foreground",
                    )}
                  >
                    {option.label}
                    <span className="tabular-nums">{counts[option.value]}</span>
                  </button>
                );
              })}
            </div>
            {isFiltered(filters) && (
              <Button
                variant="ghost"
                size="xs"
                onClick={() => {
                  setFilters(NO_FILTERS);
                }}
              >
                Clear filters <X className="size-3" aria-hidden="true" />
              </Button>
            )}
            <span className="ml-auto flex items-center gap-3">
              {streaming !== null && <LiveLabel label="trace streaming" />}
              <Button
                variant="ghost"
                size="xs"
                aria-label="Refresh the Page list"
                onClick={() => {
                  void load();
                }}
              >
                <RefreshCw className="size-3" aria-hidden="true" /> Refresh
              </Button>
            </span>
          </div>

          {loading && pages.length === 0 && <p className="text-sm text-muted-foreground">Loading the cluster…</p>}

          <div className="flex flex-col gap-2">
            {visible.map((page) => (
              <PageCard
                key={page.name}
                page={page}
                detail={details[page.name] ?? null}
                expanded={expanded === page.name}
                loading={detailLoading === page.name}
                steps={steps[page.name] ?? []}
                streaming={streaming === page.name}
                canApprove={session.canApprove}
                busy={decisionBusy === page.name}
                error={decisionErrors[page.name] === "" ? null : (decisionErrors[page.name] ?? null)}
                onToggle={() => {
                  setExpanded((current) => (current === page.name ? null : page.name));
                }}
                onApprove={(target, hash) => {
                  void decide(target, "approve", hash, "");
                }}
                onDismiss={(target, hash, reason) => {
                  void decide(target, "dismiss", hash, reason);
                }}
                onOpenCli={() => {
                  setExpanded(page.name);
                  setCliOpen(true);
                }}
              />
            ))}
          </div>

          {!loading && pages.length === 0 && (
            <div className="rounded-xl border border-dashed border-border bg-card px-4 py-10 text-center">
              <p className="text-base font-medium text-foreground">No Pages yet</p>
              <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">
                Pagerless opens one Page the first time a watched workload fails and deduplicates the rest. You
                can also open one now, from here or from a terminal.
              </p>
              <div className="mt-3 flex flex-wrap items-center justify-center gap-2">
                <Button
                  size="sm"
                  onClick={() => {
                    setPageDialogOpen(true);
                  }}
                >
                  Page a workload
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setCliOpen(true);
                  }}
                >
                  See the CLI equivalent
                </Button>
              </div>
            </div>
          )}

          {!loading && pages.length > 0 && visible.length === 0 && (
            <div className="rounded-xl border border-dashed border-border bg-card px-4 py-10 text-center">
              <p className="text-base font-medium text-foreground">Nothing in this scope</p>
              <p className="mt-1 text-sm text-muted-foreground">
                {scopeLabel(filters)} has no Page right now. Clear the filter, or activate the Pagerless
                lockup to return to the whole cluster.
              </p>
              <Button
                size="sm"
                variant="outline"
                className="mt-3"
                onClick={() => {
                  setFilters(NO_FILTERS);
                }}
              >
                Clear filters
              </Button>
            </div>
          )}
        </section>
      </main>

      <footer className="mx-auto flex w-full max-w-[1400px] flex-wrap items-center gap-2 px-3 pt-2 text-xs text-muted-foreground sm:px-5">
        <span>
          {pages.length} Pages · {proposed.length} awaiting a decision
        </span>
        <span aria-hidden="true">·</span>
        <span>
          Every action here has a <span className="font-mono text-foreground">kubectl page</span> verb.
        </span>
        {expandedPage !== null && (
          <span className="ml-auto truncate font-mono">expanded: {pageTitle(expandedPage)}</span>
        )}
      </footer>

      <CliDrawer open={cliOpen} onOpenChange={setCliOpen} page={expandedPage} />
      <PageResourceDialog
        open={pageDialogOpen}
        onOpenChange={setPageDialogOpen}
        namespaces={matrix?.namespaces.map((namespace) => namespace.name) ?? []}
        onCreated={(created) => {
          setPages((current) => [created, ...current.filter((page) => page.name !== created.name)]);
          setDetails((current) => ({ ...current, [created.name]: created }));
          setExpanded(created.name);
          setFilters(NO_FILTERS);
          void load();
        }}
      />
    </div>
  );
}
