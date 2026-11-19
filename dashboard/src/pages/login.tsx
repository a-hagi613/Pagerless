import { KeyRound } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";

import { BrandLockup } from "@/components/brand";
import { CommandBlock } from "@/components/command-block";
import { Notice } from "@/components/notices";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { ApiError, login } from "@/lib/api";
import { useSession } from "@/lib/use-session";

// auth.mode: token exchanges the bootstrap token from pagerless-admin-secret for a signed cookie.
// auth.mode: disabled never reaches this route: the server returns an anonymous session.
export function LoginPage() {
  const { refresh } = useSession();
  const navigate = useNavigate();
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await login(token.trim());
      await refresh();
      void navigate("/", { replace: true });
    } catch (cause) {
      if (cause instanceof ApiError && cause.code === "too_many_attempts") {
        setError("Too many attempts. Wait five minutes before trying again.");
      } else if (cause instanceof ApiError && cause.status === 403) {
        setError("Token sign-in is disabled on this server.");
      } else if (cause instanceof ApiError && cause.status === 401) {
        setError("That token was not accepted.");
      } else {
        setError(cause instanceof Error ? cause.message : String(cause));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex min-h-svh flex-col bg-background">
      <header className="border-b border-border bg-background">
        <div className="mx-auto flex w-full max-w-[1100px] flex-wrap items-center gap-3 px-3 py-2 sm:px-5">
          <BrandLockup />
          <div className="ml-auto">
            <ThemeToggle />
          </div>
        </div>
      </header>

      <main className="mx-auto flex w-full max-w-[560px] flex-col gap-3 px-3 py-8 sm:px-5">
        <Card>
          <CardHeader>
            <h1 className="flex items-center gap-2 text-xl font-semibold tracking-tight text-foreground">
              <KeyRound className="size-5 text-primary" aria-hidden="true" /> Sign in to Pagerless
            </h1>
            <CardDescription>
              Paste the bootstrap token the chart printed during install. It is exchanged once for a signed
              session cookie; the token itself is never stored in the browser.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form
              className="flex flex-col gap-3"
              onSubmit={(event) => {
                event.preventDefault();
                void submit();
              }}
            >
              <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="bootstrap-token">
                Bootstrap token
                <Input
                  id="bootstrap-token"
                  data-testid="login-token"
                  type="password"
                  autoComplete="off"
                  value={token}
                  onChange={(event) => {
                    setToken(event.target.value);
                  }}
                  className="font-mono text-sm"
                  placeholder="48 alphanumeric characters"
                />
              </label>

              {error !== null && <Notice tone="error">{error}</Notice>}

              <Button type="submit" size="sm" data-testid="login-submit" disabled={busy || token.trim() === ""}>
                {busy ? "Signing in…" : "Sign in"}
              </Button>

              <p className="text-xs leading-relaxed text-muted-foreground">
                Print the token with{" "}
                <span className="font-mono text-foreground">
                  kubectl -n pagerless get secret pagerless-admin-secret -o jsonpath=&#39;&#123;.data.token&#125;&#39; |
                  base64 -d
                </span>
                . Deleting that Secret ends new sign-ins without touching running sessions.
              </p>
            </form>
          </CardContent>
        </Card>

        <CommandBlock
          command={`kubectl page config set server ${window.location.origin}`}
          note="The terminal holds the same session the browser does, so both surfaces share one audit trail."
        />
      </main>
    </div>
  );
}
