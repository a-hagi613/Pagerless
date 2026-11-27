import { Check, Copy, RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";

import { InlineAlert } from "@/components/inline-alert";
import { OutcomePill } from "@/components/status-pill";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { getEvidence, UnauthenticatedError } from "@/lib/api";
import type { ToolEvidence } from "@/lib/contract";
import { elapsedLabel } from "@/lib/format";
import { useCopy } from "@/lib/use-copy";

function pretty(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

// Evidence is fetched only when an operator asks for it. The server has already sanitized the attempt;
// this view renders it as text, never as HTML.
export function EvidenceDialog({
  pageId,
  callId,
  pointer,
  open,
  onOpenChange,
}: {
  pageId: string;
  callId: string | null;
  pointer?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [evidence, setEvidence] = useState<{ callId: string; data: ToolEvidence } | null>(null);
  const [failure, setFailure] = useState<{ callId: string; message: string } | null>(null);
  const [attempt, setAttempt] = useState(0);
  const { copied, copy } = useCopy();

  useEffect(() => {
    if (!open || callId === null) {
      return;
    }
    let active = true;
    const controller = new AbortController();
    getEvidence(pageId, callId, controller.signal).then(
      (data) => {
        if (active) {
          setEvidence({ callId, data });
        }
      },
      (cause: unknown) => {
        if (
          active &&
          !(cause instanceof DOMException && cause.name === "AbortError") &&
          !(cause instanceof UnauthenticatedError)
        ) {
          setFailure({ callId, message: cause instanceof Error ? cause.message : String(cause) });
        }
      },
    );
    return () => {
      active = false;
      controller.abort();
    };
  }, [open, callId, pageId, attempt]);

  const shown = evidence !== null && evidence.callId === callId ? evidence.data : null;
  const failureForCall = failure !== null && failure.callId === callId ? failure.message : null;
  const loading = open && callId !== null && shown === null && failureForCall === null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl" aria-busy={loading}>
        <DialogHeader>
          <DialogTitle>Tool evidence</DialogTitle>
          <DialogDescription>
            Sanitized record of one tool attempt. Arguments and results are shown only after redaction and
            bounding by the service.
          </DialogDescription>
        </DialogHeader>

        {loading && (
          <p role="status" className="text-sm text-muted-foreground">
            Loading evidence…
          </p>
        )}

        {failureForCall !== null && (
          <InlineAlert
            title="Evidence could not be loaded"
            action={
              <Button
                size="xs"
                variant="outline"
                onClick={() => {
                  setFailure(null);
                  setAttempt((current) => current + 1);
                }}
              >
                <RefreshCw className="size-3" aria-hidden="true" />
                Retry
              </Button>
            }
          >
            {failureForCall}
          </InlineAlert>
        )}

        {shown !== null && (
          <div className="flex min-w-0 flex-col gap-4">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-mono text-sm font-semibold text-foreground">{shown.tool}</span>
              <OutcomePill status={shown.status} />
              {shown.durationMs >= 0 && (
                <span className="font-mono text-xs text-muted-foreground">
                  {elapsedLabel(shown.durationMs)}
                </span>
              )}
              {shown.redacted && <Badge variant="warning">redacted</Badge>}
              {shown.truncated && <Badge variant="warning">truncated</Badge>}
              <Button
                variant="ghost"
                size="xs"
                className="ml-auto"
                aria-label={copied ? "Call ID copied" : `Copy call ID ${shown.callId}`}
                onClick={() => void copy(shown.callId)}
              >
                {copied ? <Check className="size-3" aria-hidden="true" /> : <Copy className="size-3" aria-hidden="true" />}
                <span className="font-mono">{shown.callId}</span>
              </Button>
            </div>

            {pointer !== undefined && pointer !== "" && (
              <p className="rounded-md border border-border-subtle bg-panel px-2 py-1 font-mono text-xs text-muted-foreground">
                Cited location: {pointer}
              </p>
            )}

            <section aria-label="Sanitized arguments" className="min-w-0">
              <h3 className="mb-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
                Arguments
              </h3>
              <pre className="max-h-56 overflow-auto rounded-md border border-border-subtle bg-terminal p-3 font-mono text-xs leading-relaxed text-terminal-foreground">
                {pretty(shown.arguments)}
              </pre>
            </section>

            <section aria-label="Sanitized result" className="min-w-0">
              <h3 className="mb-1 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
                Result
              </h3>
              <pre className="max-h-72 overflow-auto rounded-md border border-border-subtle bg-terminal p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-terminal-foreground">
                {pretty(shown.result)}
              </pre>
            </section>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
