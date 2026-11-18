import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";

import { useCopy } from "../use-copy";

export function CommandCard({ command, note }: { command: string; note: string }) {
  const { copied, copy } = useCopy();
  return (
    <div className="overflow-hidden rounded-lg border border-border-subtle bg-[#0b1324]">
      <div className="flex items-start gap-2 px-3 py-2">
        <span className="mt-0.5 font-mono text-[12px] text-primary select-none" aria-hidden="true">
          $
        </span>
        <code className="min-w-0 flex-1 overflow-x-auto font-mono text-[12px] leading-relaxed whitespace-pre text-mint">
          {command}
        </code>
        <Button
          variant="ghost"
          size="xs"
          className="shrink-0 text-muted-foreground"
          aria-label={`Copy ${command}`}
          onClick={() => void copy(command)}
        >
          {copied ? <Check className="size-3 text-primary" /> : <Copy className="size-3" />}
        </Button>
      </div>
      <p className="border-t border-border-subtle px-3 py-1.5 text-[12px] text-muted-foreground">{note}</p>
    </div>
  );
}
