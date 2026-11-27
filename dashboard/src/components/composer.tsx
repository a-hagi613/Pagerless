import { ChevronDown, CornerDownLeft, Send } from "lucide-react";
import { useRef, useState, type FormEvent, type KeyboardEvent } from "react";

import { InlineAlert } from "@/components/inline-alert";
import { Button } from "@/components/ui/button";
import { Input, Textarea } from "@/components/ui/input";
import type { InvestigationRequest, InvestigationTarget } from "@/lib/contract";
import { newIdempotencyKey } from "@/lib/idempotency";
import { cn } from "@/lib/utils";

interface ComposerProps {
  mode: "create" | "reply";
  placeholder: string;
  submitLabel: string;
  pendingLabel: string;
  onSubmit: (request: InvestigationRequest, idempotencyKey: string) => Promise<string | null>;
  disabled?: boolean;
  disabledReason?: string;
  autoFocus?: boolean;
  onSent?: () => void;
}

interface KeyedRequest {
  signature: string;
  key: string;
}

// The composer is the only place a turn starts. Enter inserts a newline; Ctrl/Cmd+Enter submits, and the
// same Idempotency-Key is reused when the operator retries an unchanged request.
export function Composer({
  mode,
  placeholder,
  submitLabel,
  pendingLabel,
  onSubmit,
  disabled = false,
  disabledReason,
  autoFocus = false,
  onSent,
}: ComposerProps) {
  const [draft, setDraft] = useState("");
  const [namespace, setNamespace] = useState("");
  const [kind, setKind] = useState("");
  const [name, setName] = useState("");
  const [hintsOpen, setHintsOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const keyed = useRef<KeyedRequest | null>(null);
  const textarea = useRef<HTMLTextAreaElement | null>(null);
  const hintsId = mode === "create" ? "investigation-hints" : undefined;
  const hintTextId = "composer-hint";
  const errorId = "composer-error";

  const resetKey = () => {
    keyed.current = null;
  };

  const submit = async () => {
    if (disabled || submitting) {
      return;
    }
    const message = draft.trim();
    if (message === "") {
      return;
    }
    const trimmedKind = kind.trim();
    const trimmedName = name.trim();
    if ((trimmedKind === "") !== (trimmedName === "")) {
      setError("A resource hint needs both a kind and a name.");
      return;
    }
    const request: InvestigationRequest = {
      message,
      ...(namespace.trim() === "" ? {} : { namespace: namespace.trim() }),
      ...(trimmedKind === "" || trimmedName === ""
        ? {}
        : { target: { kind: trimmedKind, name: trimmedName } satisfies InvestigationTarget }),
    };
    const signature = JSON.stringify(request);
    if (keyed.current === null || keyed.current.signature !== signature) {
      keyed.current = { signature, key: newIdempotencyKey() };
    }
    setSubmitting(true);
    setError(null);
    const failure = await onSubmit(request, keyed.current.key);
    setSubmitting(false);
    if (failure === null) {
      keyed.current = null;
      setDraft("");
      onSent?.();
      textarea.current?.focus();
      return;
    }
    setError(failure);
  };

  const onFormSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    void submit();
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      void submit();
    }
  };

  return (
    <form onSubmit={onFormSubmit} className="flex flex-col gap-2" aria-label="Investigation composer">
      {mode === "create" ? (
        <div>
          <Button
            type="button"
            variant="ghost"
            size="xs"
            aria-expanded={hintsOpen}
            aria-controls={hintsId}
            onClick={() => setHintsOpen((open) => !open)}
          >
            <ChevronDown
              className={cn("size-3.5 transition-transform", hintsOpen && "rotate-180")}
              aria-hidden="true"
            />
            Namespace or resource hint (optional)
          </Button>
          <div
            id={hintsId}
            hidden={!hintsOpen}
            className="mt-2 grid gap-2 rounded-lg border border-border-subtle bg-panel p-3 sm:grid-cols-3"
          >
            <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
              Namespace
              <Input
                value={namespace}
                onChange={(event) => {
                  setNamespace(event.target.value);
                  resetKey();
                }}
                placeholder="shop"
                autoComplete="off"
                spellCheck={false}
              />
            </label>
            <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
              Resource kind
              <Input
                value={kind}
                onChange={(event) => {
                  setKind(event.target.value);
                  resetKey();
                }}
                placeholder="Deployment"
                autoComplete="off"
                spellCheck={false}
              />
            </label>
            <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
              Resource name
              <Input
                value={name}
                onChange={(event) => {
                  setName(event.target.value);
                  resetKey();
                }}
                placeholder="checkout"
                autoComplete="off"
                spellCheck={false}
              />
            </label>
          </div>
        </div>
      ) : null}

      <label htmlFor="composer-message" className="sr-only">
        {mode === "create" ? "Your question" : "Follow-up message"}
      </label>
      <Textarea
        id="composer-message"
        ref={textarea}
        value={draft}
        onChange={(event) => {
          setDraft(event.target.value);
          resetKey();
        }}
        onKeyDown={onKeyDown}
        placeholder={disabled && disabledReason !== undefined ? disabledReason : placeholder}
        rows={3}
        autoFocus={autoFocus}
        disabled={disabled || submitting}
        aria-describedby={`${hintTextId}${error !== null ? ` ${errorId}` : ""}`}
        aria-keyshortcuts="Control+Enter Meta+Enter"
        className="min-h-20"
      />

      <p id={hintTextId} className="text-xs text-muted-foreground">
        {disabled && disabledReason !== undefined
          ? disabledReason
          : "Ctrl+Enter sends. Enter adds a line."}
      </p>

      {error !== null && (
        <div id={errorId}>
          <InlineAlert
            title="The message was not sent"
            action={
              <Button type="button" size="xs" variant="outline" onClick={() => void submit()}>
                Try again
              </Button>
            }
          >
            {error}
          </InlineAlert>
        </div>
      )}

      <div className="flex items-center justify-between gap-2">
        <span className="hidden items-center gap-1 text-xs text-muted-foreground sm:inline-flex">
          <CornerDownLeft className="size-3" aria-hidden="true" />
          draft stays here if sending fails
        </span>
        <Button type="submit" size="sm" disabled={disabled || submitting || draft.trim() === ""}>
          <Send className="size-3.5" aria-hidden="true" />
          {submitting ? pendingLabel : submitLabel}
        </Button>
      </div>
      {submitting && <span role="status" className="sr-only">{pendingLabel}</span>}
    </form>
  );
}
