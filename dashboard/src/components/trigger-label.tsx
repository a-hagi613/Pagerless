import { Badge } from "./ui/badge";
import type { Trigger } from "@/lib/api";

// TriggerLabel names what started the Triage, with the pod or alert that carried it.
export function TriggerLabel({ trigger }: { trigger: Trigger }) {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1.5">
      <Badge variant="outline">{trigger.source}</Badge>
      {trigger.pod !== undefined && (
        <span className="truncate font-mono text-xs text-slate-500" title={trigger.pod}>
          {trigger.pod}
        </span>
      )}
      {trigger.pod === undefined && trigger.alertFingerprint !== undefined && (
        <span className="font-mono text-xs text-slate-500" title="Alertmanager fingerprint">
          {trigger.alertFingerprint.slice(0, 8)}
        </span>
      )}
    </div>
  );
}
