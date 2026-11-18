import { ArrowLeft } from "lucide-react";
import type { ReactNode } from "react";
import { Link, Route, Routes } from "react-router-dom";

import { Button } from "./components/ui/button";
import { signOut } from "./lib/api";
import { SessionProvider } from "./lib/session";
import { useSession } from "./lib/use-session";
import { StartTriagePage } from "./pages/start-triage";
import { TriageDetailPage } from "./pages/triage-detail";
import { TriageListPage } from "./pages/triage-list";
import { PrototypePortal } from "./prototype/PrototypePortal";

// The v1 console stays reachable on this throwaway branch so the existing
// triage pages keep working, but `/` belongs to the ticket 83 prototype. The
// prototype routes never mount the session provider, so the portal boots
// standalone without an API or an identity provider.
function LegacyConsole({ children }: { children: ReactNode }) {
  const session = useSession();
  return (
    <div className="light min-h-screen bg-background text-foreground">
      <a
        href="#content"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-white focus:px-3 focus:py-2 focus:text-sm focus:text-sky-800"
      >
        Skip to content
      </a>
      <header className="border-b border-slate-800 bg-slate-900">
        <div className="mx-auto flex h-12 max-w-[1500px] items-center gap-6 px-4">
          <Link to="/start" className="flex items-baseline gap-2 text-slate-100">
            <span className="text-base font-semibold tracking-tight">Pagerless</span>
            <span className="hidden text-xs text-slate-400 sm:inline">Triage console (v1)</span>
          </Link>
          <nav aria-label="Primary" className="flex items-center gap-4 text-sm">
            <Link to="/start" className="text-slate-300 hover:text-white">
              Start Triage
            </Link>
            <Link to="/" className="text-slate-300 hover:text-white">
              v2 prototype
            </Link>
          </nav>
          <div className="ml-auto flex items-center gap-3 text-xs text-slate-300">
            <span title={session.groups.join(", ")}>{session.username}</span>
            {session.canApprove ? (
              <span className="rounded-sm bg-emerald-900/60 px-1.5 py-0.5 text-emerald-200">approver</span>
            ) : (
              <span className="rounded-sm bg-slate-800 px-1.5 py-0.5 text-slate-400">reader</span>
            )}
            <Button
              variant="ghost"
              size="sm"
              className="text-slate-200 hover:bg-slate-800 hover:text-white"
              onClick={() => void signOut()}
            >
              Sign out
            </Button>
          </div>
        </div>
      </header>
      <main id="content" className="mx-auto max-w-[1500px] px-4 py-6">
        {children}
      </main>
    </div>
  );
}

export function App() {
  return (
    <Routes>
      <Route path="/" element={<PrototypePortal />} />
      <Route
        path="/start"
        element={
          <SessionProvider>
            <LegacyConsole>
              <StartTriagePage />
            </LegacyConsole>
          </SessionProvider>
        }
      />
      <Route
        path="/triages/:name"
        element={
          <SessionProvider>
            <LegacyConsole>
              <TriageDetailPage />
            </LegacyConsole>
          </SessionProvider>
        }
      />
      <Route
        path="/triages"
        element={
          <SessionProvider>
            <LegacyConsole>
              <TriageListPage />
            </LegacyConsole>
          </SessionProvider>
        }
      />
      <Route
        path="*"
        element={
          <div className="light grid min-h-screen place-items-center bg-background p-6">
            <div className="flex flex-col items-start gap-3">
              <p className="text-sm text-slate-600">This page does not exist.</p>
              <Link
                to="/"
                className="inline-flex items-center gap-1 text-sm text-sky-700 hover:underline"
              >
                <ArrowLeft aria-hidden="true" className="size-4" /> Back to the v2 prototype
              </Link>
            </div>
          </div>
        }
      />
    </Routes>
  );
}
