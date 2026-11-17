import { formatAge, formatTime, useNow } from "@/lib/time";

// Age shows how long ago the instant was, keeping the exact time in the tooltip.
export function Age({ at }: { at: string }) {
  const now = useNow(15_000);
  return (
    <time dateTime={at} title={formatTime(at)} className="tabular-nums text-slate-600">
      {formatAge(now, at)}
    </time>
  );
}
