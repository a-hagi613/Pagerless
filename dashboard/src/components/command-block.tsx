import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useCopy } from "@/lib/use-copy";

// A monospaced command block. Terminal surfaces stay dark in both themes: the contrast of the command
// against its own background is the same everywhere, and it reads as a terminal rather than as a card.
export function CommandBlock({
  command,
  note,
  className,
  showPrompt = true,
  flat = false,
}: {
  command: string;
  note?: string;
  className?: string;
  showPrompt?: boolean;
  // Inside a card, the block is a full-width strip rather than a second card.
  flat?: boolean;
}) {
  const { copied, copy } = useCopy();

  return (
    <div
      className={cn(
        "overflow-hidden bg-terminal",
        flat ? "border-y border-border-subtle" : "rounded-lg border border-border-subtle",
        className,
      )}
    >
      <div className="flex items-start gap-2 px-3 py-2">
        {showPrompt && (
          <span className="mt-0.5 font-mono text-xs text-primary-soft-foreground select-none" aria-hidden="true">
            $
          </span>
        )}
        <code className="min-w-0 flex-1 overflow-x-auto font-mono text-xs leading-relaxed whitespace-pre text-terminal-foreground">
          {command}
        </code>
        <Button
          variant="ghost"
          size="xs"
          className="shrink-0 text-terminal-muted hover:bg-card/20 hover:text-terminal-foreground"
          aria-label={`Copy ${command}`}
          onClick={() => void copy(command)}
        >
          {copied ? <Check className="size-3" aria-hidden="true" /> : <Copy className="size-3" aria-hidden="true" />}
          <span className="sr-only">{copied ? "Copied" : "Copy"}</span>
        </Button>
      </div>
      {note !== undefined && (
        <p className="border-t border-white/10 px-3 py-1.5 text-xs text-terminal-muted">{note}</p>
      )}
      <span className="sr-only" role="status">
        {copied ? "Command copied to the clipboard" : ""}
      </span>
    </div>
  );
}
