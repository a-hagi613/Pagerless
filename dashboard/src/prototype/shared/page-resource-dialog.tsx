import { useState } from "react";
import { Plus, Radar } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

import type { ResourceKind } from "../types";
import { usePrototype } from "../store-context";

const workloads: Record<string, { name: string; kind: ResourceKind }[]> = {
  checkout: [
    { name: "web", kind: "Deployment" },
    { name: "checkout-api", kind: "Deployment" },
    { name: "report-worker", kind: "CronJob" },
  ],
  "prod-payments": [
    { name: "payments-api", kind: "Deployment" },
    { name: "ledger", kind: "StatefulSet" },
  ],
  "ingress-system": [
    { name: "ingress-nginx", kind: "Deployment" },
    { name: "email", kind: "Deployment" },
  ],
};

// Paging a resource is the prototype's "create" flow: it starts an in-memory
// Page whose trace streams Findings on a timer.
export function PageResourceDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated?: (id: string) => void;
}) {
  const { pageResource } = usePrototype();
  const [namespace, setNamespace] = useState("checkout");
  const [workloadName, setWorkloadName] = useState("web");
  const [hint, setHint] = useState("Readiness probes fail during rollout");

  const options = workloads[namespace] ?? [];
  const selected = options.find((option) => option.name === workloadName) ?? options[0];

  const submit = () => {
    if (selected === undefined) {
      return;
    }
    const id = pageResource({ namespace, workload: selected.name, kind: selected.kind });
    onCreated?.(id);
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Radar className="size-4 text-primary" aria-hidden="true" />
            Page a resource
          </DialogTitle>
          <DialogDescription>
            The agent starts from the same signal set an alert would carry. This prototype simulates the trace
            locally; a real Page opens on the cluster and streams over SSE.
          </DialogDescription>
        </DialogHeader>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="page-namespace">
              Namespace
            </label>
            <Select
              value={namespace}
              onValueChange={(value) => {
                setNamespace(value);
                const first = workloads[value]?.[0];
                if (first !== undefined) {
                  setWorkloadName(first.name);
                }
              }}
            >
              <SelectTrigger id="page-namespace">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {Object.keys(workloads).map((name) => (
                  <SelectItem key={name} value={name}>
                    {name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex flex-col gap-1.5">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="page-workload">
              Workload
            </label>
            <Select value={selected?.name ?? ""} onValueChange={setWorkloadName}>
              <SelectTrigger id="page-workload">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {options.map((option) => (
                  <SelectItem key={option.name} value={option.name}>
                    {option.kind.toLowerCase()}/{option.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>

        <div className="flex flex-col gap-1.5">
          <label className="text-[12px] font-medium text-muted-foreground" htmlFor="page-hint">
            Hint <span className="text-muted-foreground">(optional, fed to the agent)</span>
          </label>
          <textarea
            id="page-hint"
            value={hint}
            onChange={(event) => {
              setHint(event.target.value);
            }}
            rows={2}
            className="w-full resize-none rounded-md border border-border bg-background/60 px-2.5 py-2 text-xs text-foreground placeholder:text-muted-foreground focus-visible:border-ring"
          />
        </div>

        <div className="flex flex-wrap items-center gap-2 rounded-md border border-border-subtle bg-[#0b1324] px-2.5 py-2">
          <span className="font-mono text-[12px] tracking-[0.14em] text-muted-foreground uppercase">CLI</span>
          <code className="min-w-0 flex-1 overflow-x-auto font-mono text-[12px] text-mint">
            {`kubectl page new --workload ${selected?.kind.toLowerCase() ?? "deployment"}/${selected?.name ?? ""} -n ${namespace}`}
          </code>
        </div>

        <DialogFooter>
          <Button variant="ghost" size="sm" onClick={() => { onOpenChange(false); }}>
            Cancel
          </Button>
          <Button size="sm" onClick={submit}>
            <Plus className="size-3.5" /> Page resource
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
