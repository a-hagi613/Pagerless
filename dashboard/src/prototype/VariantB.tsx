import { useMemo, useState } from "react";
import {
  ArrowUpRight,
  ChevronRight,
  CircleCheck,
  ExternalLink,
  GitPullRequest,
  Inbox,
  KeyRound,
  ListFilter,
  Play,
  Plus,
  RefreshCw,
  Settings,
  ShieldCheck,
  SquareTerminal,
  Terminal,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

import { CONNECTORS, NAMESPACES, NODES } from "./data";
import { ageLabel, shortHash } from "./format";
import { usePrototype } from "./store-context";
import type { Evidence, Page } from "./types";
import { PageStatusPill, SeverityDot } from "./shared/atoms";
import { LivePulse, PromptChip } from "./shared/brand";
import { CliDrawer } from "./shared/cli-drawer";
import { DiffView } from "./shared/diff-view";
import { OnboardingSections } from "./shared/onboarding";
import { PageResourceDialog } from "./shared/page-resource-dialog";
import { SignInDialog } from "./shared/sign-in-dialog";
import { TraceStream } from "./shared/trace";

type WorkspaceTab = "cause" | "trace" | "logs" | "settings";
type MobilePane = "feed" | "workspace" | "settings";
type FeedFilter = "all" | "critical" | "mine";

// Variant B — Split Ops Pager Console. A master-detail console with no page
// scrolling: a live feed on the left, a tabbed workspace on the right, and a
// sticky decision footer. On mobile it becomes three bottom-nav panes.
export function VariantB() {
  const { pages, session, activePageId, streamingPageIds, selectPage, createPullRequest, dismissPage } = usePrototype();
  const [tab, setTab] = useState<WorkspaceTab>("cause");
  const [mobilePane, setMobilePane] = useState<MobilePane>("workspace");
  const [filter, setFilter] = useState<FeedFilter>("all");
  const [feedQuery, setFeedQuery] = useState("");
  const [signIn, setSignIn] = useState(false);
  const [cli, setCli] = useState(false);
  const [pageDialog, setPageDialog] = useState(false);

  const activePage = pages.find((page) => page.id === activePageId) ?? pages[0] ?? null;

  const feedPages = useMemo(() => {
    const needle = feedQuery.trim().toLowerCase();
    return pages
      .filter((page) => {
        const haystack = `${page.id} ${page.title} ${page.workload} ${page.namespace}`.toLowerCase();
        const matchesQuery = needle === "" || haystack.includes(needle);
        const matchesFilter =
          filter === "all" ? true : filter === "critical" ? page.severity === "critical" : page.owner === session.name;
        return matchesQuery && matchesFilter;
      })
      .sort((left, right) => right.openedMinutes - left.openedMinutes);
  }, [feedQuery, filter, pages, session.name]);

  const incidents = pages.reduce(
    (counts, page) => {
      if (page.severity === "critical" && page.status !== "dismissed") {
        counts.critical += 1;
      }
      if (page.status === "investigating") {
        counts.watching += 1;
      }
      return counts;
    },
    { critical: 0, watching: 0 },
  );

  const showWorkspace = (pageId: string) => {
    selectPage(pageId);
    setMobilePane("workspace");
  };

  const openCli = () => {
    setCli(true);
  };

  return (
    <div className="flex h-svh flex-col overflow-hidden bg-background lg:h-[calc(100svh-64px)]">
        {/* Slim status bar: the whole console header is one 36px line. */}
        <header className="flex h-9 shrink-0 items-center gap-2 border-b border-border-subtle bg-surface-rail px-2 font-mono text-[12px] sm:px-3">
          <PromptChip className="h-4 px-1 text-[12px]" />
          <span className="text-[12px] font-semibold tracking-tight text-foreground">pagerless</span>
          <span className="hidden text-muted-foreground sm:inline">|</span>
          <span className="hidden items-center gap-1 text-muted-foreground sm:inline-flex">
            <span className="text-mint">ctx</span> devbox-sandbox
            <span className="text-muted-foreground">·</span> v1.34.2
          </span>
          <span className="ml-auto flex items-center gap-3">
            <span className="hidden items-center gap-1.5 text-muted-foreground sm:flex">
              <LivePulse label={`${String(streamingPageIds.length)} streams`} />
            </span>
            <span className="flex items-center gap-2">
              <span className="flex items-center gap-1" title="Critical Pages">
                <span className="size-1.5 rounded-full bg-destructive" aria-hidden="true" />
                {incidents.critical}
              </span>
              <span className="flex items-center gap-1" title="Pages under investigation">
                <span className="size-1.5 rounded-full bg-warning" aria-hidden="true" />
                {incidents.watching}
              </span>
              <span className="flex items-center gap-1" title="Healthy namespaces">
                <span className="size-1.5 rounded-full bg-primary" aria-hidden="true" />
                {String(NAMESPACES.filter((ns) => ns.status === "healthy").length)}
              </span>
            </span>
            <span className="hidden text-muted-foreground sm:inline">{pages.length} pages</span>
            <button
              type="button"
              onClick={() => {
                setSignIn(true);
              }}
              className="flex items-center gap-1 rounded border border-border-subtle bg-card px-1.5 py-0.5 text-muted-foreground transition-colors hover:text-foreground"
              title={`${session.role} · ${session.method}`}
            >
              <KeyRound className="size-3" aria-hidden="true" />
              <span className="max-w-24 truncate">{session.name}</span>
            </button>
          </span>
        </header>

        <div className="flex min-h-0 flex-1">
          {/* Left: incident feed. A fixed 32% column on desktop. */}
          <aside className="hidden w-[32%] min-w-[300px] max-w-[460px] shrink-0 flex-col border-r border-border-subtle bg-surface-rail/60 lg:flex">
            <div className="flex items-center gap-2 border-b border-border-subtle px-3 py-2">
              <span className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">Incident feed</span>
              <span className="font-mono text-[12px] text-muted-foreground">{feedPages.length}</span>
              <div className="ml-auto flex items-center gap-1">
                <Button variant="ghost" size="xs" onClick={openCli} aria-label="Open CLI equivalent">
                  <SquareTerminal className="size-3" />
                </Button>
                <Button variant="secondary" size="xs" onClick={() => { setPageDialog(true); }}>
                  <Plus className="size-3" /> Page
                </Button>
              </div>
            </div>

            <div className="flex flex-col gap-1.5 border-b border-border-subtle px-3 py-2">
              <div className="relative">
                <Input
                  aria-label="Filter the feed"
                  placeholder="Filter feed"
                  value={feedQuery}
                  onChange={(event) => {
                    setFeedQuery(event.target.value);
                  }}
                  className="h-7 pl-2 font-mono text-[12px]"
                />
              </div>
              <div className="flex items-center gap-1">
                {(["all", "critical", "mine"] as FeedFilter[]).map((option) => (
                  <button
                    key={option}
                    type="button"
                    onClick={() => {
                      setFilter(option);
                    }}
                    className={cn(
                      "rounded px-1.5 py-0.5 font-mono text-[12px] uppercase transition-colors",
                      filter === option
                        ? "bg-primary/15 text-primary"
                        : "text-muted-foreground hover:bg-accent hover:text-foreground",
                    )}
                  >
                    {option}
                  </button>
                ))}
                <ListFilter className="ml-auto size-3 text-muted-foreground" aria-hidden="true" />
              </div>
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto">
              <ul>
                {feedPages.map((page) => (
                  <li key={page.id}>
                    <FeedItem
                      page={page}
                      active={page.id === activePage?.id}
                      onClick={() => {
                        showWorkspace(page.id);
                      }}
                    />
                  </li>
                ))}
                {feedPages.length === 0 && (
                  <li className="px-3 py-6 text-center font-mono text-[12px] text-muted-foreground">
                    no Pages match this filter
                  </li>
                )}
              </ul>

              <div className="px-3 py-2">
                <p className="mb-1 flex items-center justify-between font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">
                  Event ticker
                  <RefreshCw className="size-2.5" aria-hidden="true" />
                </p>
                <ul className="flex flex-col gap-1">
                  {pages.slice(0, 4).map((page) => {
                    const latest = page.evidence[page.evidence.length - 1];
                    return (
                      <li key={`ticker-${page.id}`} className="flex gap-1.5 font-mono text-[12px] leading-snug">
                        <span className="shrink-0 text-muted-foreground">{latest?.at ?? "T+00:00"}</span>
                        <span className="truncate text-muted-foreground">
                          <span className="text-mint">{page.namespace}</span> {latest?.summary ?? page.signal}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              </div>
            </div>

            <div className="flex items-center gap-2 border-t border-border-subtle px-3 py-1.5 font-mono text-[12px] text-muted-foreground">
              <span>{NAMESPACES.length} ns</span>
              <span className="text-muted-foreground">·</span>
              <span>{String(NODES.length)} nodes</span>
              <span className="text-muted-foreground">·</span>
              <span className="flex items-center gap-1 text-primary">
                <span className="size-1 rounded-full bg-primary" aria-hidden="true" /> api ok
              </span>
            </div>
          </aside>

          {/* Right: full-height tabbed workspace. */}
          <section
            className={cn(
              "min-h-0 min-w-0 flex-1 flex-col",
              mobilePane === "workspace" && activePage !== null ? "flex" : "hidden lg:flex",
            )}
          >
            <Tabs
              value={tab}
              onValueChange={(value) => {
                setTab(value as WorkspaceTab);
              }}
              className="min-h-0 flex-1 gap-0"
            >
              <div className="flex shrink-0 flex-wrap items-center gap-1 border-b border-border-subtle bg-card/40 px-2 py-1.5">
                <div className="flex items-center gap-1.5 pr-2">
                  {activePage !== null && <SeverityDot severity={activePage.severity} />}
                  <span className="font-mono text-[12px] text-muted-foreground">{activePage?.id ?? "no page"}</span>
                </div>
                <TabsList className="order-last h-7 w-full min-w-0 border-0 bg-transparent p-0 sm:order-none sm:w-auto sm:flex-1">
                  <TabsTrigger value="cause" className="h-6 font-mono text-[12px]">
                    root cause &amp; diff
                  </TabsTrigger>
                  <TabsTrigger value="trace" className="h-6 font-mono text-[12px]">
                    agent trace
                  </TabsTrigger>
                  <TabsTrigger value="logs" className="h-6 font-mono text-[12px]">
                    logs &amp; events
                  </TabsTrigger>
                  <TabsTrigger value="settings" className="hidden h-6 font-mono text-[12px] sm:inline-flex">
                    settings
                  </TabsTrigger>
                </TabsList>
                <div className="flex items-center gap-1">
                  <Button variant="ghost" size="xs" className="font-mono" onClick={openCli}>
                    <Terminal className="size-3" /> cli
                  </Button>
                  <Button variant="ghost" size="xs" className="font-mono" onClick={() => { setPageDialog(true); }}>
                    <Plus className="size-3" /> page
                  </Button>
                </div>
              </div>

              {tab !== "settings" && activePage !== null && (
                <div className="shrink-0 border-b border-border-subtle px-3 py-3 sm:px-4">
                  <div className="flex flex-wrap items-center gap-2">
                    <PageStatusPill status={activePage.status} />
                    <span className="font-mono text-[12px] text-muted-foreground">
                      {activePage.id} · {activePage.trigger} · {activePage.openedAt} ({ageLabel(activePage.openedMinutes)})
                    </span>
                    <span className="ml-auto font-mono text-[12px] text-muted-foreground">
                      owner <span className="text-foreground">{activePage.owner}</span>
                    </span>
                  </div>
                  <h2 className="mt-1.5 text-[15px] font-semibold tracking-tight text-balance text-foreground">
                    {activePage.title}
                  </h2>
                  <p className="mt-0.5 font-mono text-[12px] text-muted-foreground">
                    {activePage.kind.toLowerCase()}/{activePage.namespace}/{activePage.workload}
                  </p>
                </div>
              )}

              <div className="min-h-0 flex-1 overflow-y-auto">
                <TabsContent value="cause" className="mt-0 px-3 py-3 sm:px-4">
                  {activePage === null ? (
                    <EmptyWorkspace />
                  ) : (
                    <div className="flex flex-col gap-3">
                      <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                        <SignalCell label="signal" value={activePage.signal} className="sm:col-span-2" />
                        <SignalCell
                          label="confidence"
                          value={
                            activePage.proposal === undefined
                              ? "collecting"
                              : `${String(activePage.proposal.confidence)}% · risk ${activePage.proposal.risk}`
                          }
                        />
                      </div>
                      <div className="rounded-lg border border-border-subtle bg-card px-3 py-2.5">
                        <p className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">
                          Root cause
                        </p>
                        <p className="mt-1 text-[12.5px] leading-relaxed text-foreground">{activePage.rootCause}</p>
                      </div>
                      {activePage.proposal === undefined ? (
                        <div className="rounded-lg border border-dashed border-border px-3 py-6 text-center font-mono text-[12px] text-muted-foreground">
                          no verified proposal yet — the agent is still gathering evidence
                        </div>
                      ) : (
                        <DiffView proposal={activePage.proposal} />
                      )}
                      {activePage.pullRequest !== undefined && (
                        <div className="flex items-center gap-2 rounded-lg border border-primary/25 bg-primary/5 px-3 py-2 font-mono text-[12px]">
                          <GitPullRequest className="size-3.5 text-primary" aria-hidden="true" />
                          PR #{activePage.pullRequest.number} {activePage.pullRequest.state}
                          <a
                            href={activePage.pullRequest.url}
                            target="_blank"
                            rel="noreferrer"
                            className="ml-auto inline-flex items-center gap-1 text-mint hover:underline"
                          >
                            open <ArrowUpRight className="size-3" aria-hidden="true" />
                          </a>
                        </div>
                      )}
                    </div>
                  )}
                </TabsContent>

                <TabsContent value="trace" className="mt-0 px-3 py-3 sm:px-4">
                  {activePage === null ? (
                    <EmptyWorkspace />
                  ) : (
                    <div className="flex flex-col gap-3">
                      <div className="flex flex-wrap items-center gap-2 font-mono text-[12px] text-muted-foreground">
                        <Badge variant="muted">{activePage.evidence.length} findings</Badge>
                        <Badge variant="muted">
                          {activePage.evidence.filter((line) => line.source === "Agent").length} tool chains
                        </Badge>
                        <span className="ml-auto">12,480 in · 3,912 out · $0.041</span>
                      </div>
                      <TraceStream page={activePage} />
                    </div>
                  )}
                </TabsContent>

                <TabsContent value="logs" className="mt-0 px-3 py-3 sm:px-4">
                  {activePage === null ? <EmptyWorkspace /> : <LogConsole page={activePage} />}
                </TabsContent>

                <TabsContent value="settings" className="mt-0">
                  <SettingsPane />
                </TabsContent>
              </div>

              {/* Sticky decision footer, present on every workspace tab. */}
              {tab !== "settings" && activePage !== null && (
                <footer className="flex shrink-0 flex-wrap items-center gap-2 border-t border-border-subtle bg-card/70 px-3 py-2 backdrop-blur">
                  {activePage.pullRequest === undefined ? (
                    <Button
                      size="sm"
                      disabled={activePage.status !== "ready"}
                      onClick={() => {
                        selectPage(activePage.id);
                        createPullRequest(activePage.id);
                      }}
                    >
                      <Play className="size-3.5" /> Create PR
                    </Button>
                  ) : (
                    <Button asChild size="sm">
                      <a href={activePage.pullRequest.url} target="_blank" rel="noreferrer">
                        <GitPullRequest className="size-3.5" /> PR #{activePage.pullRequest.number}
                        <ExternalLink className="size-3" />
                      </a>
                    </Button>
                  )}
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={activePage.status === "dismissed"}
                    onClick={() => {
                      dismissPage(activePage.id);
                    }}
                  >
                    Dismiss
                  </Button>
                  <span className="hidden items-center gap-1 font-mono text-[12px] text-muted-foreground sm:flex">
                    <ShieldCheck className="size-3 text-primary" aria-hidden="true" />
                    {activePage.proposal === undefined ? "no diff pinned" : `sha256:${shortHash(activePage.proposal.hash)}`}
                  </span>
                  <button
                    type="button"
                    onClick={openCli}
                    className="ml-auto hidden max-w-[45%] items-center gap-1.5 truncate rounded border border-border-subtle bg-[#0b1324] px-2 py-1 font-mono text-[12px] text-muted-foreground transition-colors hover:text-mint sm:flex"
                    title="Open the CLI equivalent"
                  >
                    <span className="text-primary">$</span>
                    kubectl page {activePage.pullRequest === undefined ? "approve" : "get"} {activePage.id}
                    {activePage.pullRequest === undefined ? " --create-pr" : " -o yaml"}
                    <ChevronRight className="size-3 shrink-0" aria-hidden="true" />
                  </button>
                </footer>
              )}
            </Tabs>
          </section>

          {/* Mobile: feed and settings as separate panes. */}
          <section
            className={cn(
              "min-h-0 min-w-0 flex-1 flex-col overflow-hidden lg:hidden",
              mobilePane === "feed" ? "flex" : "hidden",
            )}
          >
            <div className="flex items-center gap-2 border-b border-border-subtle bg-surface-rail/60 px-3 py-2">
              <span className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">Incident feed</span>
              <span className="font-mono text-[12px] text-muted-foreground">{feedPages.length}</span>
              <Button variant="secondary" size="xs" className="ml-auto" onClick={() => { setPageDialog(true); }}>
                <Plus className="size-3" /> Page
              </Button>
            </div>
            <div className="flex flex-col gap-1.5 border-b border-border-subtle px-3 py-2">
              <Input
                aria-label="Filter the feed"
                placeholder="Filter feed"
                value={feedQuery}
                onChange={(event) => {
                  setFeedQuery(event.target.value);
                }}
                className="h-7 font-mono text-[12px]"
              />
              <div className="flex items-center gap-1">
                {(["all", "critical", "mine"] as FeedFilter[]).map((option) => (
                  <button
                    key={option}
                    type="button"
                    onClick={() => {
                      setFilter(option);
                    }}
                    className={cn(
                      "rounded px-1.5 py-0.5 font-mono text-[12px] uppercase",
                      filter === option ? "bg-primary/15 text-primary" : "text-muted-foreground",
                    )}
                  >
                    {option}
                  </button>
                ))}
              </div>
            </div>
            <ul className="min-h-0 flex-1 overflow-y-auto">
              {feedPages.map((page) => (
                <li key={`mobile-${page.id}`}>
                  <FeedItem
                    page={page}
                    active={page.id === activePage?.id}
                    onClick={() => {
                      showWorkspace(page.id);
                    }}
                  />
                </li>
              ))}
            </ul>
          </section>

          <section
            className={cn(
              "min-h-0 min-w-0 flex-1 flex-col overflow-y-auto px-3 py-3 lg:hidden",
              mobilePane === "settings" ? "flex" : "hidden",
            )}
          >
            <SettingsPane />
          </section>
        </div>

        {/* Bottom navigation, mobile only. The prototype switcher sits above it. */}
        <nav
          aria-label="Console panes"
          className="flex h-12 shrink-0 items-stretch border-t border-border-subtle bg-surface-rail lg:hidden"
        >
          {(
            [
              { pane: "feed" as MobilePane, label: "Feed", icon: Inbox },
              { pane: "workspace" as MobilePane, label: "Triage", icon: Play },
              { pane: "settings" as MobilePane, label: "Setup", icon: Settings },
            ] satisfies { pane: MobilePane; label: string; icon: typeof Inbox }[]
          ).map((item) => {
            const Icon = item.icon;
            const active = mobilePane === item.pane;
            return (
              <button
                key={item.pane}
                type="button"
                onClick={() => {
                  setMobilePane(item.pane);
                }}
                aria-pressed={active}
                className={cn(
                  "flex flex-1 flex-col items-center justify-center gap-0.5 font-mono text-[12px]",
                  active ? "text-primary" : "text-muted-foreground",
                )}
              >
                <Icon className="size-4" aria-hidden="true" />
                {item.label}
              </button>
            );
          })}
        </nav>

        <SignInDialog open={signIn} onOpenChange={setSignIn} />
        <CliDrawer open={cli} onOpenChange={setCli} page={activePage} />
        <PageResourceDialog
        open={pageDialog}
        onOpenChange={setPageDialog}
        onCreated={() => {
          setMobilePane("workspace");
        }}
      />
      </div>
  );
}

function FeedItem({ page, active, onClick }: { page: Page; active: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "flex w-full items-start gap-2 border-b border-border-subtle/70 px-3 py-2 text-left transition-colors",
        active ? "bg-primary/8" : "hover:bg-accent/40",
      )}
      aria-current={active ? "true" : undefined}
    >
      <span className={cn("mt-0.5 h-8 w-0.5 shrink-0 rounded-full", active ? "bg-primary" : "bg-transparent")} aria-hidden="true" />
      <span className="min-w-0 flex-1">
        <span className="flex items-center gap-1.5 font-mono text-[12px] text-muted-foreground">
          <span className="tabular-nums">{page.openedAt}</span>
          <span className="text-muted-foreground">·</span>
          <span className="truncate text-mint">{page.namespace}</span>
          <span className="ml-auto flex items-center gap-1">
            <SeverityDot severity={page.severity} />
            {page.status === "investigating" && <span className="text-warning">live</span>}
          </span>
        </span>
        <span className="mt-0.5 block truncate text-[12px] font-medium text-foreground">{page.title}</span>
        <span className="mt-0.5 flex items-center gap-1.5">
          <span className="truncate font-mono text-[12px] text-muted-foreground">
            {page.workload} · {page.kind.toLowerCase()}
          </span>
          <span className="ml-auto shrink-0 font-mono text-[12px] text-muted-foreground">{page.id}</span>
        </span>
      </span>
    </button>
  );
}

function EmptyWorkspace() {
  return (
    <div className="grid min-h-48 place-items-center p-6 text-center">
      <div>
        <Inbox className="mx-auto size-6 text-muted-foreground" aria-hidden="true" />
        <p className="mt-2 text-xs text-muted-foreground">Select a Page from the feed.</p>
      </div>
    </div>
  );
}

function SignalCell({ label, value, className }: { label: string; value: string; className?: string }) {
  return (
    <div className={cn("rounded-lg border border-border-subtle bg-muted/30 px-2.5 py-2", className)}>
      <p className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">{label}</p>
      <p className="mt-1 font-mono text-[12px] leading-relaxed text-foreground">{value}</p>
    </div>
  );
}

function LogConsole({ page }: { page: Page }) {
  const [source, setSource] = useState<"all" | Evidence["source"]>("all");
  const sources: Evidence["source"][] = ["Kubernetes event", "Pod log", "Loki", "Prometheus", "GitHub commit", "Agent"];
  const lines = page.evidence.filter((line) => source === "all" || line.source === source);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1">
        <button
          type="button"
          onClick={() => {
            setSource("all");
          }}
          className={cn(
            "rounded border px-1.5 py-0.5 font-mono text-[12px]",
            source === "all" ? "border-primary/40 bg-primary/10 text-primary" : "border-border-subtle text-muted-foreground",
          )}
        >
          all
        </button>
        {sources.map((candidate) => (
          <button
            key={candidate}
            type="button"
            onClick={() => {
              setSource(candidate);
            }}
            className={cn(
              "rounded border px-1.5 py-0.5 font-mono text-[12px]",
              source === candidate
                ? "border-primary/40 bg-primary/10 text-primary"
                : "border-border-subtle text-muted-foreground hover:text-foreground",
            )}
          >
            {candidate.toLowerCase().replace(/\s+/gu, "-")}
          </button>
        ))}
        <span className="ml-auto font-mono text-[12px] text-muted-foreground">{lines.length} blocks</span>
      </div>

      <div className="overflow-hidden rounded-lg border border-border-subtle bg-[#080f1d]">
        <div className="flex items-center gap-1.5 border-b border-border-subtle px-2 py-1">
          <Terminal className="size-3 text-primary" aria-hidden="true" />
          <span className="font-mono text-[12px] text-muted-foreground">
            {page.namespace}/{page.workload} · tail -f
          </span>
          <LivePulse label="stream" className="ml-auto" />
        </div>
        <div className="max-h-[52vh] overflow-y-auto px-2 py-1.5">
          {lines.map((line) => (
            <div key={line.id} className="mb-2 font-mono text-[12px] leading-[1.55]">
              <p className="text-muted-foreground">
                <span className="text-sky-300">{line.source.toLowerCase().replace(/\s+/gu, "-")}</span>{" "}
                <span className="text-muted-foreground">{line.at}</span> {line.tool}
              </p>
              <p className="text-mint">{line.summary}</p>
              <pre className="whitespace-pre-wrap text-slate-400">{line.output.join("\n")}</pre>
            </div>
          ))}
          {lines.length === 0 && (
            <p className="py-4 text-center font-mono text-[12px] text-muted-foreground">no lines for this source yet</p>
          )}
        </div>
      </div>
      <p className="flex items-center gap-1.5 font-mono text-[12px] text-muted-foreground">
        <CircleCheck className="size-3 text-primary" aria-hidden="true" />
        Loki and the API server are queried read-only; nothing is written to the cluster from this pane.
      </p>
    </div>
  );
}

function SettingsPane() {
  const { settings, session, streamingPageIds } = usePrototype();
  return (
    <div className="px-3 py-3 sm:px-4">
      <div className="grid grid-cols-1 gap-3 xl:grid-cols-[260px_1fr]">
        <div className="flex flex-col gap-2">
          <div className="rounded-lg border border-border-subtle bg-card px-3 py-2.5">
            <p className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">Runtime</p>
            <dl className="mt-2 flex flex-col gap-1.5 font-mono text-[12px]">
              <div className="flex justify-between gap-2">
                <dt className="text-muted-foreground">model</dt>
                <dd className="truncate text-foreground">{settings.aiModel}</dd>
              </div>
              <div className="flex justify-between gap-2">
                <dt className="text-muted-foreground">agent</dt>
                <dd className="text-primary">ready</dd>
              </div>
              <div className="flex justify-between gap-2">
                <dt className="text-muted-foreground">streams</dt>
                <dd className="text-foreground">{streamingPageIds.length}</dd>
              </div>
              <div className="flex justify-between gap-2">
                <dt className="text-muted-foreground">session</dt>
                <dd className="text-foreground">{session.role}</dd>
              </div>
            </dl>
          </div>
          <div className="rounded-lg border border-border-subtle bg-card px-3 py-2.5">
            <p className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">Secrets</p>
            <ul className="mt-2 flex flex-col gap-1.5 text-[12px]">
              {[
                { name: "pagerless-ai", ready: true },
                { name: "pagerless-github", ready: true },
                { name: "pagerless-webhook", ready: true },
              ].map((secret) => (
                <li key={secret.name} className="flex items-center gap-1.5 font-mono text-[12px] text-muted-foreground">
                  <ShieldCheck className="size-3 text-primary" aria-hidden="true" />
                  {secret.name}
                  <span className="ml-auto text-primary">mounted</span>
                </li>
              ))}
            </ul>
          </div>
          <Separator />
          <p className="font-mono text-[12px] leading-relaxed text-muted-foreground">
            {CONNECTORS.filter((connector) => connector.state !== "available").length} connectors delivering events ·
            edits are in-memory in this prototype.
          </p>
        </div>
        <OnboardingSections />
      </div>
    </div>
  );
}
