import { useMemo, useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  CircleCheck,
  Cpu,
  ExternalLink,
  GitPullRequest,
  Network,
  Plus,
  Radar,
  Settings,
  ShieldCheck,
  SquareTerminal,
  X,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

import { CLUSTER, NAMESPACES, NODES } from "./data";
import { ageLabel } from "./format";
import { usePrototype } from "./store-context";
import type { Page, Severity } from "./types";
import {
  AgeText,
  ConfidenceBar,
  HealthPill,
  NamespaceChip,
  PageStatusPill,
  SeverityDot,
} from "./shared/atoms";
import { LivePulse, PagerMark, PromptChip } from "./shared/brand";
import { CliDrawer } from "./shared/cli-drawer";
import { DiffView } from "./shared/diff-view";
import { OnboardingSections } from "./shared/onboarding";
import { PageResourceDialog } from "./shared/page-resource-dialog";
import { SignInDialog } from "./shared/sign-in-dialog";
import { TraceStream } from "./shared/trace";

const severityOrder: Record<Severity, number> = { critical: 0, warning: 1, info: 2 };

// Variant C — Cluster Health Matrix and Guided Triage. A horizontal header, a
// two-step quickstart, a clickable health matrix, and a prioritised inbox whose
// cards expand inline to show the trace and the diff.
export function VariantC() {
  const { pages, session, selectPage, createPullRequest, dismissPage } = usePrototype();
  const [focus, setFocus] = useState("all");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [quickstart, setQuickstart] = useState(true);
  const [settings, setSettings] = useState(false);
  const [signIn, setSignIn] = useState(false);
  const [cli, setCli] = useState(false);
  const [pageDialog, setPageDialog] = useState(false);

  const expandedPage = pages.find((page) => page.id === expandedId) ?? null;

  const inbox = useMemo(() => {
    const matching = pages.filter((page) => {
      if (focus === "all") {
        return true;
      }
      if (focus === "nodes") {
        return page.signal.toLowerCase().includes("schedul") || page.kind === "CronJob";
      }
      return page.namespace === focus;
    });
    return [...matching].sort(
      (left, right) =>
        severityOrder[left.severity] - severityOrder[right.severity] ||
        right.openedMinutes - left.openedMinutes,
    );
  }, [focus, pages]);

  const pendingDecisions = pages.filter((page) => page.status === "ready");
  const mergedThisWeek = pages.filter((page) => page.pullRequest?.state === "merged");

  const expand = (page: Page) => {
    setExpandedId((current) => (current === page.id ? null : page.id));
    selectPage(page.id);
  };

  const approveFirst = () => {
    const first = pendingDecisions[0] ?? pages[0];
    if (first === undefined) {
      return;
    }
    setExpandedId(first.id);
    selectPage(first.id);
    window.setTimeout(() => {
      document.getElementById(`page-${first.id}`)?.scrollIntoView({ behavior: "smooth", block: "center" });
    }, 30);
  };

  return (
    <div className="min-h-svh bg-background pb-28">
      <header className="sticky top-0 z-30 border-b border-border-subtle bg-background/90 backdrop-blur">
        <div className="mx-auto flex h-14 w-full max-w-[1400px] items-center gap-2 px-3 sm:px-5">
          <PagerMark className="size-8" />
          <div className="min-w-0 leading-none">
            <p className="text-[14px] font-semibold tracking-tight text-foreground">Pagerless</p>
            <p className="mt-0.5 hidden font-mono text-[12px] tracking-[0.14em] text-muted-foreground uppercase sm:block">
              cluster triage
            </p>
          </div>
          <span className="mx-1 hidden h-6 w-px bg-border-subtle sm:block" aria-hidden="true" />
          <PromptChip />
          <LivePulse label="agent live" className="hidden sm:inline-flex" />

          <div className="ml-auto flex items-center gap-1.5">
            <button
              type="button"
              onClick={() => {
                setSignIn(true);
              }}
              className="hidden items-center gap-1.5 rounded-md border border-border-subtle bg-card px-2 py-1 font-mono text-[12px] text-muted-foreground transition-colors hover:text-foreground sm:flex"
              title={`${session.method} · ${session.expires}`}
            >
              <span className="grid size-4 place-items-center rounded-full bg-primary/15 font-mono text-[12px] text-primary">
                {session.name.slice(0, 2)}
              </span>
              {session.name}
            </button>
            <Button variant="ghost" size="icon-sm" aria-label="Preview sign-in" className="sm:hidden" onClick={() => { setSignIn(true); }}>
              <ShieldCheck />
            </Button>
            <Button variant="ghost" size="sm" className="hidden sm:inline-flex" onClick={() => { setCli(true); }}>
              <SquareTerminal className="size-3.5" /> CLI
            </Button>
            <Button variant="ghost" size="icon-sm" aria-label="Setup" onClick={() => { setSettings(true); }}>
              <Settings />
            </Button>
            <Button size="sm" aria-label="Page resource" onClick={() => { setPageDialog(true); }}>
              <Plus className="size-3.5" />
              <span className="hidden sm:inline">Page resource</span>
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto w-full max-w-[1400px] px-3 py-4 sm:px-5">
        {/* Guided two-step quickstart. */}
        {quickstart && (
          <section
            aria-label="Guided triage"
            className="relative mb-3 overflow-hidden rounded-xl border border-primary/25 bg-primary/5 px-3 py-3 pr-10 sm:px-4 sm:pr-10"
          >
            <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center sm:gap-3">
              <span className="flex items-center gap-1.5 font-mono text-[12px] tracking-[0.16em] text-primary uppercase">
                <Radar className="size-3.5" aria-hidden="true" /> guided triage
              </span>
              <span className="hidden h-4 w-px bg-border sm:block" aria-hidden="true" />
              <div className="flex items-center gap-2">
                <span className="grid size-5 place-items-center rounded-full border border-primary/40 bg-primary/15">
                  <CircleCheck className="size-3 text-primary" aria-hidden="true" />
                </span>
                <span className="text-[12px] text-muted-foreground">
                  <span className="font-medium text-foreground">1. Cluster connected</span>
                  <span className="hidden sm:inline">
                    {" "}
                    · {CLUSTER.name} · {CLUSTER.version}
                  </span>
                </span>
              </div>
              <div className="flex items-center gap-2">
                <span className="grid size-5 place-items-center rounded-full border border-border bg-card font-mono text-[12px] text-muted-foreground">
                  2
                </span>
                <span className="text-[12px] text-muted-foreground">
                  <span className="font-medium text-foreground">Review a Proposal</span> · {pendingDecisions.length} ready
                </span>
                <Button size="xs" variant="secondary" onClick={approveFirst}>
                  Open first PR diff <ChevronRight className="size-3" />
                </Button>
              </div>
              <button
                type="button"
                onClick={() => {
                  setQuickstart(false);
                }}
                className="absolute top-2 right-2 rounded p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                aria-label="Dismiss quickstart"
              >
                <X className="size-3.5" />
              </button>
            </div>
          </section>
        )}

        {/* Health matrix: namespaces and nodes as clickable columns. */}
        <section aria-label="Cluster health matrix" className="mb-4">
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <h1 className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">Cluster health</h1>
            <Badge variant="muted">{CLUSTER.context}</Badge>
            <span className="ml-auto flex items-center gap-3 font-mono text-[12px] text-muted-foreground">
              <span>{CLUSTER.kpi.pods} pods</span>
              <span className="text-muted-foreground">·</span>
              <span>{CLUSTER.kpi.nodes} nodes</span>
              <span className="text-muted-foreground">·</span>
              <span className="text-primary">{mergedThisWeek.length} merged</span>
            </span>
          </div>

          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4">
            {NAMESPACES.map((ns) => {
              const selected = focus === ns.name;
              return (
                <div
                  key={ns.name}
                  className={cn(
                    "flex flex-col rounded-xl border bg-card transition-colors",
                    selected ? "border-primary/60 shadow-[0_0_0_1px_var(--primary)]" : "border-border-subtle",
                  )}
                >
                  <button
                    type="button"
                    onClick={() => {
                      setFocus(selected ? "all" : ns.name);
                    }}
                    aria-pressed={selected}
                    className="flex items-center gap-2 rounded-t-xl px-3 py-2 text-left hover:bg-accent/30"
                  >
                    <span
                      className={cn(
                        "size-1.5 rounded-full",
                        ns.status === "critical" ? "bg-destructive" : ns.status === "degraded" ? "bg-warning" : "bg-primary",
                      )}
                      aria-hidden="true"
                    />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-mono text-[12px] text-foreground">{ns.name}</span>
                      <span className="block font-mono text-[12px] text-muted-foreground">
                        {ns.pods.ready}/{ns.pods.total} pods · {ns.counts.deployments} deploy · {ns.counts.cronJobs} cron
                      </span>
                    </span>
                    <HealthPill status={ns.status} />
                  </button>
                  <ul className="flex flex-1 flex-col gap-1 border-t border-border-subtle p-2">
                    {ns.workloads.map((workload) => (
                      <li key={workload.name}>
                        <button
                          type="button"
                          onClick={() => {
                            setFocus(selected ? "all" : ns.name);
                          }}
                          className="flex w-full items-center gap-1.5 rounded px-1 py-0.5 text-left font-mono text-[12px] transition-colors hover:bg-accent/40"
                          title={workload.note}
                        >
                          <span
                            className={cn(
                              "size-1.5 shrink-0 rounded-sm",
                              workload.status === "critical"
                                ? "bg-destructive"
                                : workload.status === "degraded"
                                  ? "bg-warning"
                                  : "bg-primary/60",
                            )}
                            aria-hidden="true"
                          />
                          <span className="truncate text-muted-foreground">{workload.name}</span>
                          <span className="ml-auto text-muted-foreground">{workload.ready}</span>
                        </button>
                      </li>
                    ))}
                  </ul>
                </div>
              );
            })}

            <div
              className={cn(
                "flex flex-col rounded-xl border bg-card transition-colors",
                focus === "nodes" ? "border-primary/60 shadow-[0_0_0_1px_var(--primary)]" : "border-border-subtle",
              )}
            >
              <button
                type="button"
                onClick={() => {
                  setFocus(focus === "nodes" ? "all" : "nodes");
                }}
                aria-pressed={focus === "nodes"}
                className="flex items-center gap-2 rounded-t-xl px-3 py-2 text-left hover:bg-accent/30"
              >
                <Network className="size-3.5 text-muted-foreground" aria-hidden="true" />
                <span className="min-w-0 flex-1">
                  <span className="block font-mono text-[12px] text-foreground">nodes</span>
                  <span className="block font-mono text-[12px] text-muted-foreground">
                    {NODES.filter((node) => node.status === "pressure").length} pressure · {NODES.length} nodes
                  </span>
                </span>
                <Badge variant={NODES.some((node) => node.status === "pressure") ? "warning" : "success"}>
                  {NODES.some((node) => node.status === "pressure") ? "constrained" : "fits"}
                </Badge>
              </button>
              <ul className="flex flex-1 flex-col gap-1.5 border-t border-border-subtle p-2">
                {NODES.map((node) => (
                  <li key={node.name} className="flex items-center gap-1.5">
                    <Cpu className="size-3 shrink-0 text-muted-foreground" aria-hidden="true" />
                    <span className="w-24 truncate font-mono text-[12px] text-muted-foreground">{node.name}</span>
                    <span className="h-1 flex-1 overflow-hidden rounded-full bg-border-subtle">
                      <span
                        className={cn("block h-full rounded-full", node.cpu > 90 ? "bg-destructive" : "bg-primary/70")}
                        style={{ width: `${String(node.cpu)}%` }}
                      />
                    </span>
                    <span
                      className={cn(
                        "w-8 text-right font-mono text-[12px] tabular-nums",
                        node.cpu > 90 ? "text-destructive" : "text-muted-foreground",
                      )}
                    >
                      {node.cpu}%
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          </div>
        </section>

        {/* Prioritised inbox. */}
        <section aria-label="Page inbox">
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <h2 className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">
              Inbox · {focus === "all" ? "all signals" : focus}
            </h2>
            <span className="font-mono text-[12px] text-muted-foreground">{inbox.length} pages</span>
            {focus !== "all" && (
              <button
                type="button"
                onClick={() => {
                  setFocus("all");
                }}
                className="flex items-center gap-1 rounded border border-border-subtle bg-card px-1.5 py-0.5 font-mono text-[12px] text-mint hover:border-mint/40"
              >
                clear filter <X className="size-3" />
              </button>
            )}
            <span className="ml-auto hidden font-mono text-[12px] text-muted-foreground sm:inline">
              sorted by severity, then age
            </span>
          </div>

          <div className="flex flex-col gap-2">
            {inbox.map((page) => {
              const expanded = page.id === expandedPage?.id;
              return (
                <article
                  key={page.id}
                  id={`page-${page.id}`}
                  className={cn(
                    "overflow-hidden rounded-xl border bg-card transition-colors",
                    expanded ? "border-primary/40" : "border-border-subtle",
                  )}
                >
                  <div className="flex flex-wrap items-start gap-2 px-3 py-2.5 sm:px-4">
                    <span
                      className={cn(
                        "mt-1 h-8 w-0.5 shrink-0 rounded-full",
                        page.severity === "critical"
                          ? "bg-destructive"
                          : page.severity === "warning"
                            ? "bg-warning"
                            : "bg-primary/40",
                      )}
                      aria-hidden="true"
                    />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2">
                        <SeverityDot severity={page.severity} />
                        <span className="font-mono text-[12px] text-muted-foreground">{page.id}</span>
                        <NamespaceChip namespace={page.namespace} />
                        <PageStatusPill status={page.status} />
                        {page.status === "investigating" && <LivePulse label="diagnosing" />}
                      </div>
                      <h3 className="mt-1 text-[13.5px] font-medium leading-snug text-balance text-foreground">{page.title}</h3>
                      <p className="mt-0.5 font-mono text-[12px] text-muted-foreground">{page.signal}</p>
                      <div className="mt-1.5 flex flex-wrap items-center gap-3">
                        <span className="font-mono text-[12px] text-muted-foreground">
                          {page.kind.toLowerCase()}/{page.workload}
                        </span>
                        <AgeText minutes={page.openedMinutes} />
                        {page.proposal !== undefined && <ConfidenceBar value={page.proposal.confidence} />}
                        {page.pullRequest !== undefined && (
                          <span className="font-mono text-[12px] text-mint">
                            PR #{page.pullRequest.number} {page.pullRequest.state}
                          </span>
                        )}
                      </div>
                    </div>

                    <div className="flex flex-wrap items-center gap-1.5">
                      <Button variant="outline" size="sm" onClick={() => { expand(page); }}>
                        {expanded ? "Close" : page.proposal === undefined ? "Watch trace" : "Review diff"}
                        <ChevronDown className={cn("size-3.5 transition-transform", expanded && "rotate-180")} />
                      </Button>
                      {page.pullRequest === undefined ? (
                        <Button
                          size="sm"
                          disabled={page.status !== "ready"}
                          onClick={() => {
                            createPullRequest(page.id);
                          }}
                        >
                          <GitPullRequest className="size-3.5" /> Create PR
                        </Button>
                      ) : (
                        <Button asChild size="sm" variant="secondary">
                          <a href={page.pullRequest.url} target="_blank" rel="noreferrer">
                            PR #{page.pullRequest.number} <ExternalLink className="size-3" />
                          </a>
                        </Button>
                      )}
                    </div>
                  </div>

                  {expanded && (
                    <div className="border-t border-border-subtle bg-background/40 px-3 py-3 sm:px-4">
                      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                        <div className="flex flex-col gap-2">
                          <div className="rounded-lg border border-border-subtle bg-card px-3 py-2">
                            <p className="font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase">
                              Root cause
                            </p>
                            <p className="mt-1 text-[12px] leading-relaxed text-foreground">{page.rootCause}</p>
                            <div className="mt-2 flex flex-wrap items-center gap-2">
                              <Badge variant="muted">{page.trigger}</Badge>
                              <span className="font-mono text-[12px] text-muted-foreground">
                                opened {page.openedAt} · {ageLabel(page.openedMinutes)}
                              </span>
                            </div>
                          </div>
                          <TraceStream page={page} compact />
                        </div>
                        <div className="flex flex-col gap-2">
                          {page.proposal === undefined ? (
                            <div className="rounded-lg border border-dashed border-border px-3 py-8 text-center font-mono text-[12px] text-muted-foreground">
                              no verified proposal yet
                              <span className="mt-1 block text-[12px]">the trace above keeps streaming</span>
                            </div>
                          ) : (
                            <DiffView proposal={page.proposal} />
                          )}
                          <div className="flex flex-wrap items-center gap-2">
                            {page.pullRequest === undefined ? (
                              <Button
                                size="sm"
                                disabled={page.status !== "ready"}
                                onClick={() => {
                                  createPullRequest(page.id);
                                }}
                              >
                                <GitPullRequest className="size-3.5" /> Create PR
                              </Button>
                            ) : (
                              <Button asChild size="sm">
                                <a href={page.pullRequest.url} target="_blank" rel="noreferrer">
                                  <GitPullRequest className="size-3.5" /> View PR #{page.pullRequest.number}
                                  <ExternalLink className="size-3" />
                                </a>
                              </Button>
                            )}
                            <Button
                              variant="outline"
                              size="sm"
                              disabled={page.status === "dismissed"}
                              onClick={() => {
                                dismissPage(page.id);
                                setExpandedId(null);
                              }}
                            >
                              Dismiss
                            </Button>
                            <Button variant="ghost" size="sm" className="ml-auto" onClick={() => { setCli(true); }}>
                              <SquareTerminal className="size-3.5" /> CLI
                            </Button>
                          </div>
                        </div>
                      </div>
                    </div>
                  )}
                </article>
              );
            })}

            {inbox.length === 0 && (
              <p className="rounded-xl border border-dashed border-border px-4 py-10 text-center font-mono text-[12px] text-muted-foreground">
                nothing in this namespace · clear the filter to see every Page
              </p>
            )}
          </div>
        </section>
      </main>

      <Dialog open={settings} onOpenChange={setSettings}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Settings className="size-4 text-primary" aria-hidden="true" />
              Setup
            </DialogTitle>
            <DialogDescription>
              The three steps behind this cluster: model endpoint, repository, and connectors. In a real install they
              write Secrets; here they update the prototype's memory only.
            </DialogDescription>
          </DialogHeader>
          <OnboardingSections />
        </DialogContent>
      </Dialog>

      <SignInDialog open={signIn} onOpenChange={setSignIn} />
      <CliDrawer open={cli} onOpenChange={setCli} page={expandedPage} />
      <PageResourceDialog
        open={pageDialog}
        onOpenChange={setPageDialog}
        onCreated={(id) => {
          setExpandedId(id);
          window.setTimeout(() => {
            document.getElementById(`page-${id}`)?.scrollIntoView({ behavior: "smooth", block: "center" });
          }, 60);
        }}
      />
    </div>
  );
}
