import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

import { exchangeSession, listPages, setUnauthorizedHandler, UnauthenticatedError } from "./api";
import { AuthContext, type AuthStatus, type HistoryStatus } from "./auth-context";
import type { PageSummary } from "./contract";

function messageOf(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

// AuthProvider owns the browser session and the installation's shared history. Nothing here reads or
// writes localStorage: the session is the HttpOnly cookie, and the operator token only passes through the
// exchange request body.
export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("checking");
  const [error, setError] = useState<string | null>(null);
  const [pages, setPages] = useState<PageSummary[]>([]);
  const [historyStatus, setHistoryStatus] = useState<HistoryStatus>("loading");
  const [historyError, setHistoryError] = useState<string | null>(null);
  const pagesRef = useRef<PageSummary[]>([]);

  const applyHistory = useCallback((answer: { pages: PageSummary[] }) => {
    pagesRef.current = answer.pages;
    setPages(answer.pages);
    setHistoryStatus("ready");
    setHistoryError(null);
    setStatus("authenticated");
    setError(null);
  }, []);

  const applyFailure = useCallback((cause: unknown, probe: boolean) => {
    if (cause instanceof UnauthenticatedError) {
      pagesRef.current = [];
      setPages([]);
      setStatus("required");
      setHistoryStatus("ready");
      setHistoryError(null);
      return;
    }
    const message = messageOf(cause);
    setHistoryError(message);
    if (pagesRef.current.length === 0) {
      setHistoryStatus("error");
    }
    if (probe) {
      setStatus("unreachable");
      setError(message);
    }
  }, []);

  const refreshHistory = useCallback(
    async (quiet = false) => {
      if (!quiet) {
        setHistoryStatus("loading");
      }
      try {
        applyHistory(await listPages());
      } catch (cause) {
        applyFailure(cause, false);
      }
    },
    [applyFailure, applyHistory],
  );

  const requireSession = useCallback(() => {
    pagesRef.current = [];
    setPages([]);
    setStatus("required");
    setHistoryStatus("ready");
    setHistoryError(null);
  }, []);

  const exchange = useCallback(
    async (token: string) => {
      await exchangeSession(token);
      // Only a successful authenticated read proves the cookie took effect. A refusal here is shown on the
      // sign-in card rather than silently accepting the token.
      applyHistory(await listPages());
    },
    [applyHistory],
  );

  const retryProbe = useCallback(async () => {
    setStatus("checking");
    setError(null);
    try {
      applyHistory(await listPages());
    } catch (cause) {
      applyFailure(cause, true);
    }
  }, [applyFailure, applyHistory]);

  useEffect(() => {
    setUnauthorizedHandler(requireSession);
    let active = true;
    // The first read is both the availability probe and the session check. Setters stay in the promise
    // callbacks so the effect itself has no synchronous state update.
    listPages().then(
      (answer) => {
        if (active) {
          applyHistory(answer);
        }
      },
      (cause: unknown) => {
        if (active) {
          applyFailure(cause, true);
        }
      },
    );
    return () => {
      active = false;
      setUnauthorizedHandler(null);
    };
  }, [applyFailure, applyHistory, requireSession]);

  const value = useMemo(
    () => ({
      status,
      error,
      pages,
      historyStatus,
      historyError,
      exchange,
      refreshHistory,
      retryProbe,
    }),
    [status, error, pages, historyStatus, historyError, exchange, refreshHistory, retryProbe],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
