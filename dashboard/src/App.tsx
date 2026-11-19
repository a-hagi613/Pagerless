import { Navigate, Route, Routes } from "react-router-dom";

import { BrandLockup } from "@/components/brand";
import { Notice } from "@/components/notices";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { LoginPage } from "@/pages/login";
import { PortalPage } from "@/pages/portal";
import { SettingsPage } from "@/pages/settings";
import { useSession } from "@/lib/use-session";

function BootScreen() {
  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-4">
      <div className="flex flex-col items-center gap-3">
        <BrandLockup />
        <p className="text-sm text-muted-foreground">Connecting to pagerless-server…</p>
      </div>
    </div>
  );
}

function NotFound() {
  return (
    <div className="mx-auto flex w-full max-w-[560px] flex-col gap-3 px-4 py-16">
      <div className="flex items-center justify-between gap-3">
        <BrandLockup />
        <ThemeToggle />
      </div>
      <h1 className="text-xl font-semibold text-foreground">This page does not exist</h1>
      <p className="text-sm text-muted-foreground">
        The Portal serves the cluster view at <span className="font-mono text-foreground">/</span> and
        configuration at <span className="font-mono text-foreground">/settings</span>.
      </p>
      <Button asChild size="sm" className="w-fit">
        <a href="/">Back to the cluster</a>
      </Button>
    </div>
  );
}

export function App() {
  const { session, loading, error, refresh } = useSession();

  if (loading && session === null) {
    return <BootScreen />;
  }

  if (error !== null) {
    return (
      <div className="mx-auto flex w-full max-w-[560px] flex-col gap-3 px-4 py-16">
        <Notice tone="error">The Portal could not reach pagerless-server: {error}</Notice>
        <Button
          size="sm"
          className="w-fit"
          onClick={() => {
            void refresh();
          }}
        >
          Try again
        </Button>
      </div>
    );
  }

  // No session: the server is in auth.mode=token and the cookie is absent or expired. auth.mode=disabled
  // never lands here, because the server answers with an anonymous session.
  if (session === null) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    );
  }

  return (
    <Routes>
      <Route path="/" element={<PortalPage />} />
      <Route path="/settings" element={<SettingsPage />} />
      <Route path="/login" element={<Navigate to="/" replace />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
