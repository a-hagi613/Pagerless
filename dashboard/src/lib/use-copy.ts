import { useCallback, useState } from "react";

// Copy-to-clipboard with a short confirmation, shared by every command and hash control. The
// confirmation is announced, not only coloured, and the label changes with it.
export function useCopy(resetAfterMs = 1_600) {
  const [copied, setCopied] = useState(false);

  const copy = useCallback(
    async (value: string): Promise<boolean> => {
      try {
        await navigator.clipboard.writeText(value);
        setCopied(true);
        window.setTimeout(() => {
          setCopied(false);
        }, resetAfterMs);
        return true;
      } catch {
        setCopied(false);
        return false;
      }
    },
    [resetAfterMs],
  );

  return { copied, copy };
}
