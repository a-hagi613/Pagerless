import { useState } from "react";
import { ArrowRight, Check, Copy, KeyRound, ShieldCheck } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

import { usePrototype } from "../store-context";
import type { SessionRole } from "../types";
import { useCopy } from "../use-copy";

const roles: { id: SessionRole; title: string; detail: string; scope: string }[] = [
  {
    id: "admin",
    title: "Bootstrap administrator",
    detail: "The token the server prints on first start. No identity provider, no browser round trip.",
    scope: "create PR · dismiss · settings",
  },
  {
    id: "engineer",
    title: "On-call engineer",
    detail: "A pre-issued token for the person holding the pager this week.",
    scope: "create PR · dismiss",
  },
  {
    id: "auditor",
    title: "Read-only auditor",
    detail: "Watches traces and diffs; every write is refused with a typed error.",
    scope: "read only",
  },
];

// The preview sign-in dialog shows the zero-Keycloak onboarding story without
// redirecting away from the prototype: the token is exchanged in memory.
export function SignInDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { signIn, session } = usePrototype();
  const [role, setRole] = useState<SessionRole>("admin");
  const [token, setToken] = useState("pl_bootstrap_7f3a9c2e4d81");
  const { copied, copy } = useCopy();

  const exchange = () => {
    signIn(role);
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <KeyRound className="size-4 text-primary" aria-hidden="true" />
            Preview sign-in
          </DialogTitle>
          <DialogDescription>
            Pagerless starts with a local admin token, so there is no Keycloak dependency on day one. Pick a role to
            preview the portal as that person; the token stays in memory and nothing leaves the page.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-2">
          {roles.map((candidate) => (
            <button
              key={candidate.id}
              type="button"
              onClick={() => {
                setRole(candidate.id);
              }}
              className={cn(
                "flex w-full items-start gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                role === candidate.id
                  ? "border-primary/60 bg-primary/8"
                  : "border-border-subtle bg-card hover:border-border hover:bg-accent/40",
              )}
              aria-pressed={role === candidate.id}
            >
              <span
                className={cn(
                  "mt-0.5 grid size-4 shrink-0 place-items-center rounded-full border",
                  role === candidate.id ? "border-primary bg-primary text-primary-foreground" : "border-border",
                )}
              >
                {role === candidate.id && <Check className="size-3" aria-hidden="true" />}
              </span>
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-2">
                  <span className="text-xs font-medium text-foreground">{candidate.title}</span>
                  <Badge variant="muted">{candidate.scope}</Badge>
                </span>
                <span className="mt-0.5 block text-[12px] leading-snug text-muted-foreground">{candidate.detail}</span>
              </span>
            </button>
          ))}
        </div>

        <div className="flex flex-col gap-1.5">
          <label htmlFor="preview-token" className="text-[12px] font-medium text-muted-foreground">
            Bootstrap token
          </label>
          <div className="flex items-center gap-2">
            <Input
              id="preview-token"
              type="password"
              value={token}
              onChange={(event) => {
                setToken(event.target.value);
              }}
              className="font-mono text-xs"
              autoComplete="off"
            />
            <Badge variant="mint">
              <ShieldCheck className="size-3" aria-hidden="true" /> local
            </Badge>
          </div>
          <p className="text-[12px] text-muted-foreground">
            Currently signed in as <span className="font-mono text-foreground">{session.name}</span>; exchanging a new
            token replaces this session.
          </p>
        </div>

        <div className="overflow-hidden rounded-md border border-border-subtle bg-[#0b1324]">
          <div className="flex items-center gap-1.5 border-b border-border-subtle px-2 py-1">
            <span className="font-mono text-[12px] tracking-[0.14em] text-muted-foreground uppercase">CLI parity</span>
            <Button
              variant="ghost"
              size="xs"
              className="ml-auto text-muted-foreground"
              onClick={() => void copy("kubectl page login --server https://pagerless.devbox.internal --token-stdin")}
            >
              {copied ? <Check className="size-3 text-primary" /> : <Copy className="size-3" />}
              {copied ? "copied" : "copy"}
            </Button>
          </div>
          <pre className="overflow-x-auto px-2 py-1.5 font-mono text-[12px] leading-relaxed text-slate-300">
            {`$ kubectl page login --server https://pagerless.devbox.internal \\
    --token-stdin < pl-bootstrap.token`}
          </pre>
        </div>

        <DialogFooter>
          <Button variant="ghost" size="sm" onClick={() => { onOpenChange(false); }}>
            Cancel
          </Button>
          <Button size="sm" onClick={exchange}>
            Exchange token <ArrowRight className="size-3.5" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
