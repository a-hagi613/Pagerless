import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { RefreshCw, Search } from "lucide-react";

import { Age } from "../components/age";
import { EmptyNotice, ErrorNotice } from "../components/notices";
import { PhaseBadge } from "../components/phase-badge";
import { TriggerLabel } from "../components/trigger-label";
import { Button } from "../components/ui/button";
import { Card, CardContent } from "../components/ui/card";
import { Input } from "../components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../components/ui/select";
import { useDebouncedValue } from "../hooks/use-debounced-value";
import { listPages, type Phase, type PageFilters, type PageSummary, type TriggerSource } from "../lib/api";

const phases: Phase[] = [
  "Pending",
  "Investigating",
  "Proposed",
  "Approved",
  "PullRequestOpen",
  "Verifying",
  "Recovered",
  "NotRecovered",
  "Diagnosed",
  "Rejected",
  "Expired",
  "Invalid",
  "Failed",
  "Superseded",
];

const triggers: TriggerSource[] = ["Event", "Condition", "Alertmanager", "Grafana", "Datadog", "SNS", "CLI", "Portal", "API"];

const ranges = [
  { value: "any", label: "Any time" },
  { value: "1h", label: "Last hour" },
  { value: "24h", label: "Last 24 hours" },
  { value: "168h", label: "Last 7 days" },
];

const refreshInterval = 5_000;

// initialFilters reads the filters from the URL, so a list view can be linked.
function initialFilters(params: URLSearchParams): PageFilters {
  return {
    phase: params.get("phase") ?? undefined,
    target: params.get("target") ?? undefined,
    trigger: params.get("trigger") ?? undefined,
    since: params.get("since") ?? undefined,
  };
}

function filterParams(filters: PageFilters): URLSearchParams {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "") {
      params.set(key, value);
    }
  }
  return params;
}

export function PageListPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [filters, setFilters] = useState<PageFilters>(() => initialFilters(searchParams));
  const [pages, setPages] = useState<PageSummary[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const debouncedTarget = useDebouncedValue(filters.target ?? "", 300);

  useEffect(() => {
    setSearchParams(filterParams(filters), { replace: true });
  }, [filters, setSearchParams]);

  const query = useMemo<PageFilters>(
    () => ({
      phase: filters.phase,
      target: debouncedTarget === "" ? undefined : debouncedTarget,
      trigger: filters.trigger,
      since: filters.since,
    }),
    [debouncedTarget, filters.phase, filters.since, filters.trigger],
  );

  useEffect(() => {
    const controller = new AbortController();
    let disposed = false;
    const load = async (first: boolean) => {
      if (first) {
        setLoading(true);
      }
      try {
        const rows = await listPages(query, controller.signal);
        if (!disposed) {
          setPages(rows);
          setError(null);
        }
      } catch (cause) {
        if (disposed || (cause instanceof DOMException && cause.name === "AbortError")) {
          return;
        }
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        if (!disposed && first) {
          setLoading(false);
        }
      }
    };
    void load(true);
    const timer = window.setInterval(() => {
      void load(false);
    }, refreshInterval);
    return () => {
      disposed = true;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [query]);

  const update = useCallback((change: Partial<PageFilters>) => {
    setFilters((current) => ({ ...current, ...change }));
  }, []);

  const clear = useCallback(() => {
    setFilters({});
  }, []);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight text-slate-900">Pages</h1>
          <p className="text-sm text-slate-500">
            Investigations of failing Targets, most recent first. The list refreshes every few seconds.
          </p>
        </div>
        <div className="flex items-center gap-2 text-xs text-slate-500">
          <Link to="/start">
            <Button size="sm">Start Page</Button>
          </Link>
          <RefreshCw aria-hidden="true" className="size-3.5" />
          <span aria-live="polite">
            {loading ? "Loading" : `${String(pages.length)} Pages`}
          </span>
        </div>
      </div>

      <Card>
        <CardContent className="grid gap-3 py-3 sm:grid-cols-2 lg:grid-cols-4">
          <div className="flex flex-col gap-1">
            <label htmlFor="phase-filter" className="text-xs font-medium text-slate-600">
              Phase
            </label>
            <Select
              value={filters.phase ?? "any"}
              onValueChange={(value) => {
                update({ phase: value === "any" ? undefined : value });
              }}
            >
              <SelectTrigger id="phase-filter">
                <SelectValue placeholder="Any phase" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="any">Any phase</SelectItem>
                {phases.map((phase) => (
                  <SelectItem key={phase} value={phase}>
                    {phase}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="target-filter" className="text-xs font-medium text-slate-600">
              Target
            </label>
            <div className="relative">
              <Search
                aria-hidden="true"
                className="pointer-events-none absolute top-2.5 left-2.5 size-4 text-slate-400"
              />
              <Input
                id="target-filter"
                className="pl-8"
                placeholder="Target name"
                value={filters.target ?? ""}
                onChange={(event) => {
                  update({ target: event.target.value });
                }}
              />
            </div>
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="trigger-filter" className="text-xs font-medium text-slate-600">
              Trigger source
            </label>
            <Select
              value={filters.trigger ?? "any"}
              onValueChange={(value) => {
                update({ trigger: value === "any" ? undefined : value });
              }}
            >
              <SelectTrigger id="trigger-filter">
                <SelectValue placeholder="Any trigger" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="any">Any trigger</SelectItem>
                {triggers.map((trigger) => (
                  <SelectItem key={trigger} value={trigger}>
                    {trigger}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="range-filter" className="text-xs font-medium text-slate-600">
              Time range
            </label>
            <Select
              value={filters.since ?? "any"}
              onValueChange={(value) => {
                update({ since: value === "any" ? undefined : value });
              }}
            >
              <SelectTrigger id="range-filter">
                <SelectValue placeholder="Any time" />
              </SelectTrigger>
              <SelectContent>
                {ranges.map((range) => (
                  <SelectItem key={range.value} value={range.value}>
                    {range.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </CardContent>
      </Card>

      {error !== null && <ErrorNotice>{error}</ErrorNotice>}

      {!loading && pages.length === 0 ? (
        <div className="flex flex-col items-start gap-3">
          <EmptyNotice>No Pages match these filters.</EmptyNotice>
          <Button variant="outline" size="sm" onClick={clear}>
            Clear filters
          </Button>
        </div>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white shadow-xs">
          <table className="w-full border-collapse text-sm">
            <caption className="sr-only">Pages, most recent first</caption>
            <thead>
              <tr className="border-b border-slate-200 bg-slate-50 text-left text-xs font-semibold tracking-wide text-slate-600 uppercase">
                <th scope="col" className="px-4 py-2.5">
                  Phase
                </th>
                <th scope="col" className="px-4 py-2.5">
                  Target
                </th>
                <th scope="col" className="px-4 py-2.5">
                  Trigger
                </th>
                <th scope="col" className="px-4 py-2.5 text-right">
                  Age
                </th>
              </tr>
            </thead>
            <tbody>
              {pages.map((page) => (
                <tr key={page.name} className="border-b border-slate-100 last:border-0 hover:bg-slate-50">
                  <td className="px-4 py-2.5 align-top">
                    <div className="flex items-center gap-2">
                      <PhaseBadge phase={page.phase} />
                      {page.source === "history" && (
                        <span className="text-xs text-slate-400" title="The Page resource was deleted; the row comes from history">
                          history
                        </span>
                      )}
                    </div>
                  </td>
                  <td className="px-4 py-2.5 align-top">
                    <Link
                      to={`/pages/${encodeURIComponent(page.name)}`}
                      className="font-medium text-sky-800 hover:underline"
                    >
                      {page.target.namespace}/{page.target.name}
                    </Link>
                    <div className="font-mono text-xs text-slate-500">{page.name}</div>
                  </td>
                  <td className="px-4 py-2.5 align-top">
                    <TriggerLabel trigger={page.trigger} />
                  </td>
                  <td className="px-4 py-2.5 text-right align-top whitespace-nowrap">
                    <Age at={page.createdAt} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
