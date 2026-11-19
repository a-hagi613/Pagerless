import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";

import { getSession, UnauthenticatedError } from "./api";
import { SessionContext } from "./session-context";
import type { Session } from "./types";

// Loads the session once before the Portal renders. A 401 is not an error: it is the signed-out state,
// and the application shows the sign-in route. auth.mode=disabled arrives as an authenticated session,
// so the Portal opens with no token at all.
export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const next = await getSession();
      setSession(next);
      setError(null);
    } catch (cause) {
      if (cause instanceof UnauthenticatedError) {
        setSession(null);
        setError(null);
      } else {
        setError(cause instanceof Error ? cause.message : String(cause));
      }
    } finally {
      setLoading(false);
    }
  }, []);

  // The first read happens in the effect so the render tree never sets state during mount. A manual
  // refresh (after sign-in, or a 401) goes through the same loader.
  useEffect(() => {
    let active = true;
    getSession().then(
      (next) => {
        if (active) {
          setSession(next);
          setError(null);
          setLoading(false);
        }
      },
      (cause: unknown) => {
        if (!active) {
          return;
        }
        if (cause instanceof UnauthenticatedError) {
          setSession(null);
          setError(null);
        } else {
          setError(cause instanceof Error ? cause.message : String(cause));
        }
        setLoading(false);
      },
    );
    return () => {
      active = false;
    };
  }, []);

  const value = useMemo(() => ({ session, loading, error, refresh }), [session, loading, error, refresh]);

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}
