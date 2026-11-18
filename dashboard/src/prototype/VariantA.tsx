import { useMemo, useState } from "react";
import {
  ArrowRight,
  ArrowUpRight,
  Boxes,
  ChevronRight,
  CircleCheck,
  CircleExclamationPoint,
  Clock,
  ExternalLink,
  GitPullRequest,
  Inbox,
  LayoutDashboard,
  Link2,
  Menu,
  Plus,
  Radar,
  Search,
  Sparkles,
  SquareTerminal,
  TriangleExclamationPoint,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

import { CLI_SECTIONS, CONNECTORS, NAMESPACES } from "./data";
import { usePrototype } from "./store-context";
import type { Page } from "./types";
import { Wordmark } from "./shared/brand";
import { CommandCard } from "./shared/command-card";
import { CliDrawer } from "./shared/cli-drawer";
import { DiffView } from "./shared/diff-view";
import { OnboardingSections } from "./shared/onboarding";
import { PageResourceDialog } from "./shared/page-resource-dialog";
import { SignInDialog } from "./shared/sign-in-dialog";
import { TraceStream } from "./shared/trace";
import {
  AgeText,
  ConfidenceBar,
  HealthPill,
  NamespaceChip,
  PageStatusPill,
  SeverityDot,
  WorkloadRef,
} from "./shared/atoms";

type View = "overview" | "pages" | "connectors" | "onboarding" | "cli";

const navItems: { view: View; label: string; icon: typeof LayoutDashboard }[] = [
  { view: "overview", label: "Overview", icon: LayoutDashboard },
  { view: "pages", label: "Pages", icon: Inbox },
  { view: "connectors", label: "Connectors", icon: Link2 },
  { view: "onboarding", label: "Onboarding", icon: Sparkles },
  { view: "cli", label: "CLI Parity", icon: SquareTerminal },
];

const viewTitles: Record<View, { title: string; description: string }> = {
  overview: { title: "Overview", description: "Cluster health, open Pages and what needs a decision." },
  pages: { title: "Pages", description: "Every incident the agent is working, newest first." },
  connectors: { title: "Connectors", description: "Signals that can open a Page." },
  onboarding: { title: "Onboarding", description: "Connect the agent, GitHub and your alert sources." },
  cli: { title: "CLI parity", description: "Every portal action has a kubectl page verb." },
};

// Variant A — Command Sidebar. A shadcn dashboard-01 shell: collapsible rail,
// KPI strip, filterable table, and a slide-over Sheet for the trace and diff.
export function VariantA() {
  const { pages, session, activePageId, createPullRequest, dismissPage, selectPage, streamingPageIds } = usePrototype();
  const [view, setView] = useState<View>("overview");
  const [query, setQuery] = useState("");
  const [namespace, setNamespace] = useState("all");
  const [status, setStatus] = useState("all");
  const [mobileNav, setMobileNav] = useState(false);
  const [signIn, setSignIn] = useState(false);
  const [cli, setCli] = useState(false);
  const [pageDialog, setPageDialog] = useState(false);
  const [detailId, setDetailId] = useState<string | null>(null);

  const activePage = pages.find((page) => page.id === detailId) ?? null;
  const openPages = pages.filter((page) => page.status !== "recovered" && page.status !== "dismissed");
  const critical = openPages.filter((page) => page.severity === "critical");
  const awaiting = pages.filter((page) => page.status === "pr-open");
  const recovered = pages.filter((page) => page.status === "recovered");

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return pages.filter((page) => {
      const haystack = `${page.id} ${page.title} ${page.workload} ${page.namespace}`.toLowerCase();
      const matchesQuery = needle === "" || haystack.includes(needle);
      const matchesNamespace = namespace === "all" || page.namespace === namespace;
      const matchesStatus =
        status === "all" ||
        (status === "open" && (page.status === "ready" || page.status === "investigating" || page.status === "pending")) ||
        (status === "ready" && page.status === "ready") ||
        (status === "pr-open" && page.status === "pr-open") ||
        (status === "recovered" && page.status === "recovered");
      return matchesQuery && matchesNamespace && matchesStatus;
    });
  }, [namespace, pages, query, status]);

  const navigate = (next: View) => {
    setView(next);
    setMobileNav(false);
  };

  const openDetail = (page: Page) => {
    selectPage(page.id);
    setDetailId(page.id);
  };

  return (
    <TooltipProvider>
      <SidebarProvider>
      <Sidebar aria-label="Primary">
        <SidebarHeader className="border-b border-border-subtle">
          <div className="flex w-full items-center gap-2">
            <Wordmark />
            <SidebarTrigger className="ml-auto" />
          </div>
        </SidebarHeader>

        <SidebarContent>
          <SidebarGroup>
            <SidebarGroupLabel>Console</SidebarGroupLabel>
            <SidebarMenu>
              {navItems.map((item) => {
                const Icon = item.icon;
                const count = item.view === "pages" ? openPages.length : undefined;
                return (
                  <SidebarMenuItem key={item.view}>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <SidebarMenuButton
                          active={view === item.view}
                          onClick={() => {
                            navigate(item.view);
                          }}
                        >
                          <Icon className="size-4 shrink-0" aria-hidden="true" />
                          <span className="min-w-0 flex-1 truncate">{item.label}</span>
                          {count !== undefined && (
                            <span className="rounded bg-muted px-1 font-mono text-[12px] text-muted-foreground">
                              {count}
                            </span>
                          )}
                        </SidebarMenuButton>
                      </TooltipTrigger>
                      <TooltipContent>{item.label}</TooltipContent>
                    </Tooltip>
                  </SidebarMenuItem>
                );
              })}
            </SidebarMenu>
          </SidebarGroup>

          <SidebarGroup>
            <SidebarGroupLabel>Namespaces</SidebarGroupLabel>
            <SidebarMenu>
              {NAMESPACES.map((ns) => (
                <SidebarMenuItem key={ns.name}>
                  <SidebarMenuButton
                    onClick={() => {
                      setNamespace(ns.name);
                      navigate("pages");
                    }}
                  >
                    <span
                      className={cn(
                        "size-1.5 shrink-0 rounded-full",
                        ns.status === "critical" ? "bg-destructive" : ns.status === "degraded" ? "bg-warning" : "bg-primary",
                      )}
                      aria-hidden="true"
                    />
                    <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{ns.name}</span>
                    <span className="font-mono text-[12px] text-muted-foreground">
                      {ns.pods.ready}/{ns.pods.total}
                    </span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        </SidebarContent>

        <SidebarFooter>
          <div className="rounded-lg border border-border-subtle bg-card px-2 py-2">
            <div className="flex items-center gap-2">
              <span className="grid size-6 shrink-0 place-items-center rounded-full bg-primary/15 font-mono text-[12px] text-primary">
                {session.name.slice(0, 2)}
              </span>
              <div className="min-w-0 flex-1">
                <p className="truncate text-[12px] font-medium text-foreground">{session.name}</p>
                <p className="font-mono text-[12px] text-muted-foreground">{session.role} · {session.expires}</p>
              </div>
            </div>
            <Button
              variant="ghost"
              size="xs"
              className="mt-1 w-full justify-center"
              onClick={() => {
                setSignIn(true);
              }}
            >
              Preview sign-in
            </Button>
          </div>
        </SidebarFooter>
      </Sidebar>

      <SidebarInset>
        <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-border-subtle bg-background/85 px-3 backdrop-blur sm:px-4">
          <Button
            variant="ghost"
            size="icon-sm"
            className="lg:hidden"
            aria-label="Open navigation"
            onClick={() => {
              setMobileNav(true);
            }}
          >
            <Menu />
          </Button>
          <div className="flex min-w-0 flex-col leading-tight">
            <span className="truncate text-[13px] font-semibold text-foreground">{viewTitles[view].title}</span>
            <span className="hidden truncate text-[12px] text-muted-foreground sm:block">
              {viewTitles[view].description}
            </span>
          </div>

          <div className="ml-auto flex items-center gap-1.5">
            <div className="relative hidden md:block">
              <Search className="pointer-events-none absolute top-2 left-2 size-3.5 text-muted-foreground" aria-hidden="true" />
              <Input
                aria-label="Search Pages"
                placeholder="Search Pages"
                value={query}
                onChange={(event) => {
                  setQuery(event.target.value);
                  setView("pages");
                }}
                className="h-8 w-44 pl-7 text-xs"
              />
              <kbd className="pointer-events-none absolute top-1.5 right-1.5 rounded border border-border-subtle px-1 font-mono text-[12px] text-muted-foreground">
                /
              </kbd>
            </div>
            <Badge variant="mint" className="hidden sm:inline-flex">
              <span className="size-1.5 rounded-full bg-primary" aria-hidden="true" />
              {streamingPageIds.length} streaming
            </Badge>
            <Button variant="outline" size="sm" className="hidden sm:inline-flex" onClick={() => { setCli(true); }}>
              <SquareTerminal className="size-3.5" /> CLI
            </Button>
            <Button size="sm" onClick={() => { setPageDialog(true); }}>
              <Plus className="size-3.5" />
              <span className="hidden sm:inline">Page resource</span>
              <span className="sm:hidden">Page</span>
            </Button>
          </div>
        </header>

        <main className="mx-auto w-full max-w-[1440px] flex-1 px-3 py-4 pb-28 sm:px-5">
          {view === "overview" && (
            <div className="flex flex-col gap-4">
              <div className="grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-4">
                <Stat label="Open Pages" value={String(openPages.length)} sub="2 need a decision" icon={Inbox} />
                <Stat
                  label="Critical"
                  value={String(critical.length)}
                  sub="Sev 1 · customer-facing"
                  icon={CircleExclamationPoint}
                  tone="danger"
                />
                <Stat label="PRs awaiting merge" value={String(awaiting.length)} sub="draft · hash pinned" icon={GitPullRequest} tone="mint" />
                <Stat label="Recovered this week" value={String(recovered.length)} sub="verified by the agent" icon={CircleCheck} />
              </div>

              <div className="grid grid-cols-1 gap-3 xl:grid-cols-3">
                <div className="flex flex-col gap-3 xl:col-span-2">
                  <div className="flex items-center justify-between">
                    <h2 className="text-[12px] font-medium tracking-[0.14em] text-muted-foreground uppercase">Namespaces</h2>
                    <Button variant="ghost" size="xs" onClick={() => { navigate("pages"); }}>
                      All Pages <ArrowRight className="size-3" />
                    </Button>
                  </div>
                  <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
                    {NAMESPACES.map((ns) => (
                      <button
                        key={ns.name}
                        type="button"
                        onClick={() => {
                          setNamespace(ns.name);
                          navigate("pages");
                        }}
                        className="group rounded-xl border border-border-subtle bg-card p-3 text-left transition-colors hover:border-border hover:bg-accent/30"
                      >
                        <div className="flex items-center justify-between gap-2">
                          <span className="font-mono text-[12px] text-foreground">{ns.name}</span>
                          <HealthPill status={ns.status} />
                        </div>
                        <p className="mt-1 text-[12px] leading-snug text-muted-foreground">{ns.role}</p>
                        <div className="mt-2 flex items-center gap-2">
                          <span className="h-1 flex-1 overflow-hidden rounded-full bg-border-subtle">
                            <span
                              className={cn(
                                "block h-full rounded-full",
                                ns.pods.ready === ns.pods.total ? "bg-primary" : "bg-warning",
                              )}
                              style={{ width: `${String((ns.pods.ready / ns.pods.total) * 100)}%` }}
                            />
                          </span>
                          <span className="font-mono text-[12px] text-muted-foreground tabular-nums">
                            {ns.pods.ready}/{ns.pods.total} pods
                          </span>
                        </div>
                        <ul className="mt-2 flex flex-col gap-1 border-t border-border-subtle pt-2">
                          {ns.workloads.slice(0, 3).map((workload) => (
                            <li key={workload.name} className="flex items-center gap-1.5 font-mono text-[12px]">
                              <span
                                className={cn(
                                  "size-1 rounded-full",
                                  workload.status === "critical"
                                    ? "bg-destructive"
                                    : workload.status === "degraded"
                                      ? "bg-warning"
                                      : "bg-primary/70",
                                )}
                                aria-hidden="true"
                              />
                              <span className="truncate text-muted-foreground">{workload.name}</span>
                              <span className="ml-auto text-muted-foreground">{workload.ready}</span>
                            </li>
                          ))}
                        </ul>
                        <span className="mt-2 flex items-center gap-1 text-[12px] text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100">
                          Open in Pages <ChevronRight className="size-3" />
                        </span>
                      </button>
                    ))}
                  </div>

                  <div className="flex items-center justify-between pt-1">
                    <h2 className="text-[12px] font-medium tracking-[0.14em] text-muted-foreground uppercase">Needs a decision</h2>
                    <span className="font-mono text-[12px] text-muted-foreground">sorted by severity</span>
                  </div>
                  <div className="flex flex-col gap-2">
                    {openPages.map((page) => (
                      <button
                        key={page.id}
                        type="button"
                        onClick={() => {
                          openDetail(page);
                        }}
                        className="flex flex-wrap items-center gap-2 rounded-lg border border-border-subtle bg-card px-3 py-2.5 text-left transition-colors hover:border-border hover:bg-accent/30"
                      >
                        <SeverityDot severity={page.severity} />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-[12.5px] font-medium text-foreground">{page.title}</span>
                          <span className="mt-0.5 flex flex-wrap items-center gap-2">
                            <WorkloadRef page={page} />
                            <span className="font-mono text-[12px] text-muted-foreground">{page.id}</span>
                          </span>
                        </span>
                        <PageStatusPill status={page.status} />
                        <ChevronRight className="size-4 text-muted-foreground" aria-hidden="true" />
                      </button>
                    ))}
                  </div>
                </div>

                <div className="flex flex-col gap-3">
                  <Card>
                    <CardHeader className="flex-row items-center justify-between">
                      <CardTitle>Connector health</CardTitle>
                      <Badge variant="success">4 live</Badge>
                    </CardHeader>
                    <CardContent className="flex flex-col gap-2">
                      {CONNECTORS.filter((connector) => connector.state !== "available").map((connector) => (
                        <div key={connector.id} className="flex items-center gap-2">
                          <span
                            className={cn(
                              "size-1.5 rounded-full",
                              connector.state === "attention" ? "bg-warning" : "bg-primary",
                            )}
                            aria-hidden="true"
                          />
                          <span className="min-w-0 flex-1 truncate text-[12px] text-foreground">{connector.name}</span>
                          <span className="font-mono text-[12px] text-muted-foreground">{connector.meta.split(" · ")[0]}</span>
                        </div>
                      ))}
                      <Button variant="outline" size="sm" className="mt-1" onClick={() => { navigate("connectors"); }}>
                        Manage connectors
                      </Button>
                    </CardContent>
                  </Card>

                  <Card>
                    <CardHeader>
                      <CardTitle>Recent activity</CardTitle>
                    </CardHeader>
                    <CardContent className="flex flex-col gap-3">
                      {pages.slice(0, 4).map((page) => (
                        <div key={page.id} className="flex items-start gap-2">
                          <Clock className="mt-0.5 size-3 shrink-0 text-muted-foreground" aria-hidden="true" />
                          <div className="min-w-0 flex-1">
                            <p className="truncate text-[12px] text-foreground">{page.signal}</p>
                            <p className="mt-0.5 font-mono text-[12px] text-muted-foreground">
                              {page.id} · {page.namespace} · <AgeText minutes={page.openedMinutes} />
                            </p>
                          </div>
                        </div>
                      ))}
                    </CardContent>
                  </Card>

                  <div className="rounded-xl border border-primary/25 bg-primary/5 p-3">
                    <p className="flex items-center gap-1.5 text-[12px] font-medium text-foreground">
                      <Radar className="size-3.5 text-primary" aria-hidden="true" /> Zero-Keycloak start
                    </p>
                    <p className="mt-1 text-[12px] leading-relaxed text-muted-foreground">
                      A bootstrap token signs the first administrator in. Point alerts at the webhook and Pages begin.
                    </p>
                    <Button variant="outline" size="xs" className="mt-2" onClick={() => { navigate("onboarding"); }}>
                      See the three steps
                    </Button>
                  </div>
                </div>
              </div>
            </div>
          )}

          {view === "pages" && (
            <div className="flex flex-col gap-3">
              <div className="flex flex-wrap items-center gap-2 rounded-xl border border-border-subtle bg-card p-2.5">
                <div className="relative min-w-0 flex-1 sm:max-w-xs">
                  <Search className="pointer-events-none absolute top-2.5 left-2.5 size-3.5 text-muted-foreground" aria-hidden="true" />
                  <Input
                    aria-label="Filter Pages"
                    placeholder="Filter by workload, id or namespace"
                    value={query}
                    onChange={(event) => {
                      setQuery(event.target.value);
                    }}
                    className="h-9 pl-8 text-xs"
                  />
                </div>
                <Select value={status} onValueChange={setStatus}>
                  <SelectTrigger className="h-9 w-[150px] text-xs" aria-label="Filter by status">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All states</SelectItem>
                    <SelectItem value="open">Open</SelectItem>
                    <SelectItem value="ready">Ready to create PR</SelectItem>
                    <SelectItem value="pr-open">PR open</SelectItem>
                    <SelectItem value="recovered">Recovered</SelectItem>
                  </SelectContent>
                </Select>
                <Select value={namespace} onValueChange={setNamespace}>
                  <SelectTrigger className="h-9 w-[170px] text-xs" aria-label="Filter by namespace">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All namespaces</SelectItem>
                    {NAMESPACES.map((ns) => (
                      <SelectItem key={ns.name} value={ns.name}>
                        {ns.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <span className="ml-auto font-mono text-[12px] text-muted-foreground">
                  {filtered.length} of {pages.length} Pages
                </span>
              </div>

              <div className="hidden overflow-hidden rounded-xl border border-border-subtle bg-card sm:block">
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead className="pl-4">Page</TableHead>
                      <TableHead>Workload</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Confidence</TableHead>
                      <TableHead>Opened</TableHead>
                      <TableHead className="pr-4 text-right">
                        <span className="sr-only">Open detail</span>
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filtered.map((page) => (
                      <TableRow
                        key={page.id}
                        className="cursor-pointer"
                        onClick={() => {
                          openDetail(page);
                        }}
                      >
                        <TableCell className="max-w-[320px] pl-4 whitespace-normal">
                          <div className="flex items-start gap-2">
                            <SeverityDot severity={page.severity} className="mt-1.5" />
                            <div className="min-w-0">
                              <p className="truncate text-[12.5px] font-medium text-foreground">{page.title}</p>
                              <p className="mt-0.5 font-mono text-[12px] text-muted-foreground">{page.id}</p>
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col gap-1">
                            <span className="text-[12px] text-foreground">
                              {page.kind.toLowerCase()} · {page.workload}
                            </span>
                            <span className="font-mono text-[12px] text-muted-foreground">{page.namespace}</span>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5">
                            <PageStatusPill status={page.status} />
                            {page.status === "investigating" && (
                              <span className="font-mono text-[12px] text-warning">live</span>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
                          {page.proposal === undefined ? (
                            <span className="font-mono text-[12px] text-muted-foreground">—</span>
                          ) : (
                            <ConfidenceBar value={page.proposal.confidence} />
                          )}
                        </TableCell>
                        <TableCell>
                          <AgeText minutes={page.openedMinutes} />
                        </TableCell>
                        <TableCell className="pr-4 text-right">
                          <ChevronRight className="ml-auto size-4 text-muted-foreground" aria-hidden="true" />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                {filtered.length === 0 && (
                  <p className="px-4 py-10 text-center text-xs text-muted-foreground">
                    No Pages match these filters. Clear the search or pick another namespace.
                  </p>
                )}
              </div>

              <div className="flex flex-col gap-2 sm:hidden">
                {filtered.map((page) => (
                  <button
                    key={page.id}
                    type="button"
                    onClick={() => {
                      openDetail(page);
                    }}
                    className="rounded-lg border border-border-subtle bg-card p-3 text-left"
                  >
                    <div className="flex items-start gap-2">
                      <SeverityDot severity={page.severity} className="mt-1.5" />
                      <span className="min-w-0 flex-1 text-[12.5px] font-medium text-foreground">{page.title}</span>
                      <ChevronRight className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                    </div>
                    <div className="mt-2 flex flex-wrap items-center gap-2">
                      <NamespaceChip namespace={page.namespace} />
                      <PageStatusPill status={page.status} />
                      <AgeText minutes={page.openedMinutes} />
                    </div>
                  </button>
                ))}
                {filtered.length === 0 && (
                  <p className="rounded-lg border border-border-subtle bg-card px-4 py-8 text-center text-xs text-muted-foreground">
                    No Pages match these filters.
                  </p>
                )}
              </div>
            </div>
          )}

          {view === "connectors" && (
            <div className="flex flex-col gap-3">
              <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
                {CONNECTORS.map((connector) => (
                  <Card key={connector.id}>
                    <CardContent className="flex flex-col gap-2">
                      <div className="flex items-start gap-2">
                        <span className="grid size-8 place-items-center rounded-lg border border-border-subtle bg-muted/60">
                          {connector.group === "Logs" ? (
                            <Boxes className="size-4 text-mint" aria-hidden="true" />
                          ) : connector.group === "Metrics" ? (
                            <CircleExclamationPoint className="size-4 text-warning" aria-hidden="true" />
                          ) : (
                            <Link2 className="size-4 text-primary" aria-hidden="true" />
                          )}
                        </span>
                        <div className="min-w-0 flex-1">
                          <p className="text-[12px] font-medium text-foreground">{connector.name}</p>
                          <p className="font-mono text-[12px] text-muted-foreground">{connector.group}</p>
                        </div>
                        <Badge
                          variant={
                            connector.state === "connected" || connector.state === "native"
                              ? "success"
                              : connector.state === "attention"
                                ? "warning"
                                : "muted"
                          }
                        >
                          {connector.state}
                        </Badge>
                      </div>
                      <p className="text-[12px] leading-relaxed text-muted-foreground">{connector.detail}</p>
                      <p className="font-mono text-[12px] text-muted-foreground">{connector.meta}</p>
                      <div className="mt-auto flex items-center gap-2 pt-1">
                        <Button
                          variant="outline"
                          size="xs"
                          onClick={() => {
                            navigate("onboarding");
                          }}
                        >
                          {connector.state === "available" ? "Connect" : "Configure"}
                        </Button>
                        {connector.state !== "available" && (
                          <span className="flex items-center gap-1 font-mono text-[12px] text-primary">
                            <CircleCheck className="size-3" aria-hidden="true" /> verified
                          </span>
                        )}
                      </div>
                    </CardContent>
                  </Card>
                ))}
              </div>
              <p className="flex items-center gap-1.5 text-[12px] text-muted-foreground">
                <TriangleExclamationPoint className="size-3.5 text-warning" aria-hidden="true" />
                Connector changes are prototype-only; the real server writes them to a Secret.
              </p>
            </div>
          )}

          {view === "onboarding" && <OnboardingSections className="max-w-3xl" />}

          {view === "cli" && (
            <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
              {CLI_SECTIONS.map((section) => (
                <Card key={section.label}>
                  <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                      <SquareTerminal className="size-3.5 text-primary" aria-hidden="true" />
                      {section.label}
                    </CardTitle>
                    <p className="text-[12px] text-muted-foreground">{section.parity}</p>
                  </CardHeader>
                  <CardContent className="flex flex-col gap-2">
                    {section.commands.map((item) => (
                      <CommandCard key={item.command} command={item.command} note={item.note} />
                    ))}
                  </CardContent>
                </Card>
              ))}
              <Card className="lg:col-span-2">
                <CardContent className="flex flex-wrap items-center gap-3">
                  <div className="min-w-0 flex-1">
                    <p className="text-[12px] font-medium text-foreground">Prefer a drawer while you work?</p>
                    <p className="text-[12px] text-muted-foreground">
                      The CLI equivalent opens beside any Page and mirrors the buttons in its footer.
                    </p>
                  </div>
                  <Button variant="secondary" size="sm" onClick={() => { setCli(true); }}>
                    <SquareTerminal className="size-3.5" /> Open CLI drawer
                  </Button>
                </CardContent>
              </Card>
            </div>
          )}
        </main>
      </SidebarInset>

      {/* Mobile navigation drawer: the same sidebar composition, in a Sheet. */}
      <Sheet open={mobileNav} onOpenChange={setMobileNav}>
        <SheetContent side="left" className="w-[86%] max-w-xs p-0">
          <SheetHeader className="border-b border-border-subtle">
            <SheetTitle className="sr-only">Navigation</SheetTitle>
            <Wordmark />
          </SheetHeader>
          <div className="flex flex-1 flex-col gap-3 overflow-y-auto p-3">
            <SidebarMenu>
              {navItems.map((item) => {
                const Icon = item.icon;
                return (
                  <SidebarMenuItem key={item.view}>
                    <SidebarMenuButton
                      active={view === item.view}
                      onClick={() => {
                        navigate(item.view);
                      }}
                    >
                      <Icon className="size-4" aria-hidden="true" />
                      <span className="flex-1 text-left">{item.label}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                );
              })}
            </SidebarMenu>
            <div className="mt-auto rounded-lg border border-border-subtle bg-card p-2">
              <p className="truncate text-[12px] text-foreground">{session.name}</p>
              <Button variant="ghost" size="xs" className="mt-1 w-full" onClick={() => { setMobileNav(false); setSignIn(true); }}>
                Preview sign-in
              </Button>
            </div>
          </div>
        </SheetContent>
      </Sheet>

      <Sheet
        open={activePage !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDetailId(null);
          }
        }}
      >
        <SheetContent className="w-full sm:max-w-[620px]">
          {activePage !== null && (
            <>
              <SheetHeader>
                <div className="flex flex-wrap items-center gap-2">
                  <SeverityDot severity={activePage.severity} />
                  <span className="font-mono text-[12px] tracking-[0.14em] text-muted-foreground uppercase">
                    {activePage.id} · {activePage.trigger}
                  </span>
                  <PageStatusPill status={activePage.status} />
                </div>
                <SheetTitle className="pr-6 text-[15px] leading-snug text-balance">{activePage.title}</SheetTitle>
                <p className="text-[12px] text-muted-foreground">
                  {activePage.kind.toLowerCase()}/{activePage.namespace}/{activePage.workload} · opened{" "}
                  {activePage.openedAt} · owner {activePage.owner}
                </p>
              </SheetHeader>

              <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
                <Tabs defaultValue="cause">
                  <TabsList>
                    <TabsTrigger value="cause">Root cause</TabsTrigger>
                    <TabsTrigger value="trace">Agent trace</TabsTrigger>
                    <TabsTrigger value="diff">Diff</TabsTrigger>
                  </TabsList>

                  <TabsContent value="cause" className="mt-3 flex flex-col gap-3">
                    <div className="rounded-lg border border-border-subtle bg-muted/30 p-3">
                      <p className="text-[12px] tracking-[0.14em] text-muted-foreground uppercase">Signal</p>
                      <p className="mt-1 font-mono text-[12px] leading-relaxed text-foreground">{activePage.signal}</p>
                    </div>
                    <div>
                      <p className="text-[12px] tracking-[0.14em] text-muted-foreground uppercase">Root cause</p>
                      <p className="mt-1 text-[12px] leading-relaxed text-foreground">{activePage.rootCause}</p>
                    </div>
                    {activePage.proposal !== undefined && (
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge variant="mint">{activePage.proposal.action}</Badge>
                        <ConfidenceBar value={activePage.proposal.confidence} />
                        <span className="font-mono text-[12px] text-muted-foreground">
                          risk {activePage.proposal.risk}
                        </span>
                      </div>
                    )}
                    {activePage.pullRequest !== undefined && (
                      <div className="flex items-center gap-2 rounded-lg border border-primary/25 bg-primary/5 px-3 py-2">
                        <GitPullRequest className="size-3.5 text-primary" aria-hidden="true" />
                        <span className="text-[12px] text-foreground">
                          Pull request #{activePage.pullRequest.number} {activePage.pullRequest.state}
                        </span>
                        <a
                          href={activePage.pullRequest.url}
                          target="_blank"
                          rel="noreferrer"
                          className="ml-auto inline-flex items-center gap-1 text-[12px] text-mint hover:underline"
                        >
                          open <ExternalLink className="size-3" aria-hidden="true" />
                        </a>
                      </div>
                    )}
                    <div>
                      <p className="mb-1.5 text-[12px] tracking-[0.14em] text-muted-foreground uppercase">
                        Proposed change
                      </p>
                      {activePage.proposal === undefined ? (
                        <p className="rounded-lg border border-dashed border-border px-3 py-4 text-[12px] text-muted-foreground">
                          No proposal yet. The agent is still collecting Findings for this Page.
                        </p>
                      ) : (
                        <DiffView proposal={activePage.proposal} showChecks={false} />
                      )}
                    </div>
                  </TabsContent>

                  <TabsContent value="trace" className="mt-3">
                    <TraceStream page={activePage} />
                  </TabsContent>

                  <TabsContent value="diff" className="mt-3">
                    {activePage.proposal === undefined ? (
                      <p className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-[12px] text-muted-foreground">
                        A diff appears once the agent has a verified Proposal.
                      </p>
                    ) : (
                      <DiffView proposal={activePage.proposal} />
                    )}
                  </TabsContent>
                </Tabs>
              </div>

              <footer className="flex flex-wrap items-center gap-2 border-t border-border-subtle px-5 py-3">
                {activePage.pullRequest !== undefined ? (
                  <Button asChild size="sm">
                    <a href={activePage.pullRequest.url} target="_blank" rel="noreferrer">
                      <GitPullRequest className="size-3.5" /> View PR #{activePage.pullRequest.number}
                      <ArrowUpRight className="size-3" />
                    </a>
                  </Button>
                ) : (
                  <Button
                    size="sm"
                    disabled={activePage.status !== "ready"}
                    onClick={() => {
                      createPullRequest(activePage.id);
                    }}
                  >
                    <GitPullRequest className="size-3.5" /> Create PR
                  </Button>
                )}
                <Button
                  variant="outline"
                  size="sm"
                  disabled={activePage.status === "dismissed"}
                  onClick={() => {
                    dismissPage(activePage.id);
                    setDetailId(null);
                  }}
                >
                  Dismiss
                </Button>
                <Button variant="ghost" size="sm" className="ml-auto" onClick={() => { setCli(true); }}>
                  <SquareTerminal className="size-3.5" /> CLI
                </Button>
              </footer>
            </>
          )}
        </SheetContent>
      </Sheet>

      <SignInDialog open={signIn} onOpenChange={setSignIn} />
      <CliDrawer open={cli} onOpenChange={setCli} page={activePage} />
      <PageResourceDialog
        open={pageDialog}
        onOpenChange={setPageDialog}
        onCreated={(id) => {
          setDetailId(id);
        }}
      />
      <span className="sr-only" aria-live="polite">
        {activePageId === null ? "" : `Selected Page ${activePageId}`}
      </span>
      </SidebarProvider>
    </TooltipProvider>
  );
}

function Stat({
  label,
  value,
  sub,
  icon: Icon,
  tone,
}: {
  label: string;
  value: string;
  sub: string;
  icon: typeof Inbox;
  tone?: "danger" | "mint";
}) {
  return (
    <Card className="gap-0 py-0">
      <CardContent className="flex items-start gap-2 px-3 py-3">
        <div className="min-w-0 flex-1">
          <p className="font-mono text-[12px] tracking-[0.12em] text-muted-foreground uppercase">{label}</p>
          <p className="mt-1 text-2xl leading-none font-semibold text-foreground tabular-nums">{value}</p>
          <p className="mt-1 text-[12px] leading-snug text-muted-foreground">{sub}</p>
        </div>
        <span
          className={cn(
            "grid size-7 place-items-center rounded-md border",
            tone === "danger"
              ? "border-destructive/30 bg-destructive/10 text-destructive"
              : tone === "mint"
                ? "border-mint/25 bg-mint-surface text-mint"
                : "border-border-subtle bg-muted/60 text-muted-foreground",
          )}
        >
          <Icon className="size-3.5" aria-hidden="true" />
        </span>
      </CardContent>
    </Card>
  );
}
