import { useEffect, useState, type ReactNode } from "react";

import { ErrorNotice } from "../components/notices";
import { getSession, type Session } from "./api";
import { SessionContext } from "./session-context";

// SessionProvider loads the signed-in user once, before the application
// renders. An expired session sends the browser to the server's sign-in route.
export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    getSession(controller.signal).then(
      (value) => {
        setSession(value);
      },
      (cause: unknown) => {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        setError(cause instanceof Error ? cause.message : String(cause));
      },
    );
    return () => {
      controller.abort();
    };
  }, []);

  if (error !== null) {
    return (
      <div className="mx-auto max-w-xl p-6">
        <ErrorNotice>{error}</ErrorNotice>
      </div>
    );
  }
  if (session === null) {
    return <p className="p-6 text-sm text-slate-500">Loading your session…</p>;
  }
  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>;
}
