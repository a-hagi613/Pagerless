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
                <Button
                  key={`${citation.callId}-${pointer ?? ""}`}
                  variant="outline"
                  size="xs"
                  className="font-mono"
                  onClick={() => {
                    onEvidence(citation);
                  }}
                  aria-label={
                    pointer === undefined
                      ? `Open evidence for ${citation.callId}`
                      : `Open evidence for ${citation.callId} at ${pointer}`
                  }
                >
                  <FileSearch className="size-3" aria-hidden="true" />
                  {citation.callId}
                  {pointer !== undefined && <span className="text-muted-foreground">{pointer}</span>}
                </Button>
              );
            })}
          </div>
        )}
      </article>
    </li>
  );
}
