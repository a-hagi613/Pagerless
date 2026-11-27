import { KeyRound } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent } from "react";

import { BrandLockup } from "@/components/brand";
import { InlineAlert } from "@/components/inline-alert";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api";
import { useAuth } from "@/lib/use-auth";

// The browser trades the installer's operator token for an HttpOnly session cookie. The field is cleared
// before the request settles; the token never enters localStorage, a URL or an error message.
export function SignInPage() {
  const { exchange } = useAuth();
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const input = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    input.current?.focus();
  }, []);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const value = token.trim();
    if (value === "") {
      setError("Enter the operator token from your installation.");
      return;
    }
    setSubmitting(true);
    setError(null);
    setToken("");
    try {
      await exchange(value);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        setError("That token was not accepted. Check the value supplied by the installer and try again.");
      } else {
        setError(cause instanceof Error ? cause.message : String(cause));
      }
      input.current?.focus();
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="min-h-svh bg-background">
      <div className="mx-auto flex w-full max-w-[560px] flex-wrap items-center justify-between gap-2 px-4 py-4">
        <BrandLockup />
        <ThemeToggle />
      </div>
      <main id="main" className="mx-auto flex w-full max-w-[420px] flex-col gap-4 px-4 pt-8">
        <div className="flex flex-col gap-1.5">
          <span className="flex size-10 items-center justify-center rounded-lg border border-border bg-terminal text-terminal-foreground">
            <KeyRound className="size-5" aria-hidden="true" />
          </span>
          <h1 className="mt-2 text-xl font-semibold tracking-tight text-foreground">Sign in to Pagerless</h1>
          <p className="text-sm leading-relaxed text-muted-foreground">
            Paste the operator token supplied by your installer. The browser keeps it only as a private
            session cookie; it is never written to local storage.
          </p>
        </div>

        <form onSubmit={(event) => void submit(event)} className="flex flex-col gap-3 rounded-xl border border-border bg-card p-4">
          <label htmlFor="operator-token" className="text-sm font-medium text-foreground">
            Operator token
          </label>
          <Input
            id="operator-token"
            ref={input}
            type="password"
            value={token}
            onChange={(event) => setToken(event.target.value)}
            autoComplete="off"
            spellCheck={false}
            placeholder="Paste the token"
            aria-describedby="operator-token-hint"
            disabled={submitting}
          />
          <p id="operator-token-hint" className="text-xs text-muted-foreground">
            The token is sent once to this installation and exchanged for a SameSite=Strict session cookie.
          </p>
          {error !== null && <InlineAlert title="Sign-in failed">{error}</InlineAlert>}
          <Button type="submit" disabled={submitting || token.trim() === ""} className="w-fit">
            {submitting ? "Signing in…" : "Sign in"}
          </Button>
        </form>
      </main>
    </div>
  );
}
