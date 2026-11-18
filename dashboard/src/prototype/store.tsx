import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

import {
  CLUSTER,
  INITIAL_PAGES,
  INITIAL_SESSION,
  NEW_PAGE_EVIDENCE,
  NEW_PAGE_TEMPLATE,
  SCHEDULING_FOLLOWUP,
  SETTINGS_DEFAULTS,
} from "./data";
import { PrototypeContext } from "./store-context";
import type { Evidence, Page, ProtoSession, ResourceKind, SessionRole, Settings, Toast } from "./types";

const ROLE_NAMES: Record<SessionRole, string> = {
  admin: "admin@pagerless.local",
  engineer: "ada@pagerless.local",
  auditor: "audit@pagerless.local",
};

const STREAM_INTERVAL = 1_800;

interface PageState {
  pages: Page[];
  pending: Record<string, Evidence[]>;
}

// PrototypeProvider keeps every Page, Proposal and trace in memory. Nothing is
// persisted: reloading restores the scripted fixture state, which is what makes
// the three variants comparable.
export function PrototypeProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<PageState>({
    pages: INITIAL_PAGES,
    pending: { "pg-7d10": SCHEDULING_FOLLOWUP },
  });
  const [activePageId, setActivePageId] = useState<string | null>(INITIAL_PAGES[0]?.id ?? null);
  const [session, setSession] = useState<ProtoSession>(INITIAL_SESSION);
  const [settings, setSettings] = useState<Settings>({ ...SETTINGS_DEFAULTS });
  const [toasts, setToasts] = useState<Toast[]>([]);
  const toastId = useRef(0);

  const notify = useCallback((title: string, detail: string, tone: Toast["tone"] = "success") => {
    toastId.current += 1;
    setToasts((current) => [...current.slice(-2), { id: toastId.current, title, detail, tone }]);
  }, []);

  const dismissToast = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  useEffect(() => {
    if (toasts.length === 0) {
      return;
    }
    const timer = window.setTimeout(() => {
      setToasts((current) => current.slice(1));
    }, 4_200);
    return () => {
      window.clearTimeout(timer);
    };
  }, [toasts]);

  // Advance one scripted Finding per streaming Page every couple of seconds. A
  // Page whose queue drains and that already holds a Proposal becomes
  // actionable, so a newly paged resource turns into a reviewable diff.
  useEffect(() => {
    const timer = window.setInterval(() => {
      setState((current) => {
        const ids = Object.keys(current.pending);
        if (ids.length === 0) {
          return current;
        }
        const pending: Record<string, Evidence[]> = { ...current.pending };
        const arrivals = new Map<string, Evidence>();
        for (const pageId of ids) {
          const [first, ...rest] = pending[pageId] ?? [];
          if (first === undefined) {
            delete pending[pageId];
            continue;
          }
          arrivals.set(pageId, first);
          if (rest.length === 0) {
            delete pending[pageId];
          } else {
            pending[pageId] = rest;
          }
        }
        if (arrivals.size === 0) {
          return current;
        }
        const pages = current.pages.map((page) => {
          const arrival = arrivals.get(page.id);
          if (arrival === undefined) {
            return page;
          }
          return {
            ...page,
            evidence: [...page.evidence, arrival],
            status: pending[page.id] === undefined && page.proposal !== undefined ? "ready" : page.status,
          };
        });
        return { pages, pending };
      });
    }, STREAM_INTERVAL);
    return () => {
      window.clearInterval(timer);
    };
  }, []);

  const { pages, pending } = state;
  const streamingPageIds = useMemo(() => Object.keys(pending), [pending]);

  const selectPage = useCallback((id: string | null) => {
    setActivePageId(id);
  }, []);

  const createPullRequest = useCallback(
    (id: string) => {
      setState((current) => ({
        ...current,
        pages: current.pages.map((page) => {
          if (page.id !== id || page.proposal === undefined) {
            return page;
          }
          const number = page.pullRequest?.number ?? 418;
          const evidence: Evidence = {
            id: `${id}-pr-${String(number)}`,
            at: "now",
            source: "GitHub commit",
            tool: "gh pr create --draft",
            summary: `Draft PR ${String(number)} opened on ${page.proposal.branch}`,
            output: [
              `branch  ${page.proposal.branch} (from ${page.proposal.baseCommit})`,
              `file    ${page.proposal.path}`,
              `hash    ${page.proposal.hash}`,
              "review  requested from @a-hagi613",
            ],
          };
          return {
            ...page,
            status: "pr-open",
            pullRequest: {
              number,
              url: `https://github.com/a-hagi613/pagerless-sandbox/pull/${String(number)}`,
              state: "open",
            },
            evidence: [...page.evidence, evidence],
          };
        }),
      }));
      const proposalPath = pages.find((candidate) => candidate.id === id)?.proposal?.path ?? "values.yaml";
      notify(`Draft PR opened for ${id}`, `${proposalPath} · awaiting review · hash pinned`);
    },
    [notify, pages],
  );

  const dismissPage = useCallback(
    (id: string) => {
      setState((current) => ({
        ...current,
        pages: current.pages.map((page) => (page.id === id ? { ...page, status: "dismissed" } : page)),
      }));
      setActivePageId((current) => (current === id ? null : current));
      notify(`Page ${id} dismissed`, "The audit trail keeps the trace and the proposed diff.", "info");
    },
    [notify],
  );

  const pageResource = useCallback(
    (target: { namespace: string; workload: string; kind: ResourceKind }) => {
      const id = `pg-${Math.random().toString(16).slice(2, 6)}`;
      const page: Page = {
        id,
        title: NEW_PAGE_TEMPLATE.title,
        namespace: target.namespace,
        workload: target.workload,
        kind: target.kind,
        severity: "warning",
        status: "investigating",
        openedAt: "now",
        openedMinutes: 0,
        trigger: "Manual page",
        signal: NEW_PAGE_TEMPLATE.signal,
        cluster: CLUSTER.name,
        owner: "you",
        rootCause: NEW_PAGE_TEMPLATE.rootCause,
        proposal: NEW_PAGE_TEMPLATE.proposal,
          evidence: [
          {
            id: `${id}-1`,
            at: "T+00:01",
            source: "Kubernetes event",
            tool: `kubectl describe ${target.kind.toLowerCase()} -n ${target.namespace} ${target.workload}`,
            summary: "Readiness probe failures recorded during the last rollout",
            output: [
              "Warning  Unhealthy  Readiness probe failed: context deadline exceeded",
              "Warning  Unhealthy  Readiness probe failed: context deadline exceeded",
              "Normal   Pulled     Container image already present on machine",
            ],
          },
        ],
      };
      setState((current) => ({
        pages: [page, ...current.pages],
        pending: { ...current.pending, [id]: NEW_PAGE_EVIDENCE },
      }));
      setActivePageId(id);
      notify(
        `Paged ${target.namespace}/${target.workload}`,
        "The agent is streaming Findings; a Proposal follows.",
        "info",
      );
      return id;
    },
    [notify],
  );

  const signIn = useCallback(
    (role: SessionRole) => {
      setSession({
        role,
        name: ROLE_NAMES[role],
        method: role === "admin" ? "bootstrap-token" : "preview-token",
        expires: "in 23h",
      });
      notify("Signed in with a local token", `${ROLE_NAMES[role]} · no identity provider involved`);
    },
    [notify],
  );

  const updateSettings = useCallback((patch: Partial<Settings>) => {
    setSettings((current) => ({ ...current, ...patch }));
  }, []);

  const value = useMemo(
    () => ({
      pages,
      clusterName: CLUSTER.name,
      session,
      settings,
      toasts,
      activePageId,
      streamingPageIds,
      selectPage,
      createPullRequest,
      dismissPage,
      pageResource,
      signIn,
      updateSettings,
      notify,
      dismissToast,
    }),
    [
      pages,
      session,
      settings,
      toasts,
      activePageId,
      streamingPageIds,
      selectPage,
      createPullRequest,
      dismissPage,
      pageResource,
      signIn,
      updateSettings,
      notify,
      dismissToast,
    ],
  );

  return <PrototypeContext.Provider value={value}>{children}</PrototypeContext.Provider>;
}
