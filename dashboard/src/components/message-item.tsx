import { FileSearch } from "lucide-react";

import { Button } from "@/components/ui/button";
import type { Citation, PageMessage } from "@/lib/contract";
import { citationPointer } from "@/lib/contract";
import { timeOfDay } from "@/lib/format";

// Messages are plain text on purpose: model output is untrusted, so it is rendered as text and never as
// HTML. Citations are buttons that open the sanitized tool evidence they point at.
export function MessageItem({
  message,
  onEvidence,
}: {
  message: PageMessage;
  onEvidence: (citation: Citation) => void;
}) {
  const assistant = message.role === "assistant";
  return (
    <li className="min-w-0">
      <article
        aria-label={assistant ? "Pagerless answer" : "Your message"}
        className={
          assistant
            ? "rounded-lg border border-border-subtle bg-card px-3 py-2.5"
            : "rounded-lg border border-primary/30 bg-primary-soft px-3 py-2.5"
        }
      >
        <div className="mb-1 flex items-center gap-2">
          <p className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">
            {assistant ? "Pagerless" : "You"}
          </p>
          <time dateTime={message.createdAt} className="ml-auto font-mono text-xs text-muted-foreground">
            {timeOfDay(message.createdAt)}
          </time>
        </div>
        <p className="text-sm leading-relaxed break-words whitespace-pre-wrap text-foreground">
          {message.content}
        </p>
        {message.citations.length > 0 && (
          <div className="mt-2 flex flex-wrap items-center gap-1.5" aria-label="Citations">
            {message.citations.map((citation) => {
              const pointer = citationPointer(citation);
              return (
                // A JSON pointer has no break opportunities of its own. Let a long citation wrap
                // inside the conversation instead of widening the page on a narrow screen: an
                // overflowing page makes the browser grow the layout viewport, which in turn
                // stretches fixed dialogs past the physical screen.
                <Button
                  key={`${citation.callId}-${pointer ?? ""}`}
                  variant="outline"
                  size="xs"
                  className="h-auto max-w-full min-h-7 min-w-0 shrink items-start justify-start gap-1 py-1 text-left font-mono whitespace-normal [overflow-wrap:anywhere]"
                  onClick={() => {
                    onEvidence(citation);
                  }}
                  aria-label={
                    pointer === undefined
                      ? `Open evidence for ${citation.callId}`
                      : `Open evidence for ${citation.callId} at ${pointer}`
                  }
                >
                  <FileSearch className="mt-0.5 size-3" aria-hidden="true" />
                  <span className="min-w-0 [overflow-wrap:anywhere]">{citation.callId}</span>
                  {pointer !== undefined && (
                    <span className="min-w-0 text-muted-foreground [overflow-wrap:anywhere]">
                      {pointer}
                    </span>
                  )}
                </Button>
              );
            })}
          </div>
        )}
      </article>
    </li>
  );
}
