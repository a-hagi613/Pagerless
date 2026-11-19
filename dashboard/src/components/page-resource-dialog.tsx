import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/native-select";
import { CommandBlock } from "@/components/command-block";
import { Notice } from "@/components/notices";
import { createPage } from "@/lib/api";
import type { PageDetail, TargetKind } from "@/lib/types";

const kinds: TargetKind[] = [
  "Deployment",
  "StatefulSet",
  "DaemonSet",
  "Job",
  "CronJob",
  "Pod",
  "PersistentVolumeClaim",
  "Node",
];

const kindPrefix: Record<TargetKind, string> = {
  Deployment: "deployment",
  StatefulSet: "statefulset",
  DaemonSet: "daemonset",
  Job: "job",
  CronJob: "cronjob",
  Pod: "pod",
  PersistentVolumeClaim: "pvc",
  Node: "node",
};

// The Portal's equivalent of `kubectl page <TARGET>`. It opens a Page; it never mutates the cluster.
export function PageResourceDialog({
  open,
  onOpenChange,
  namespaces,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  namespaces: string[];
  onCreated: (page: PageDetail) => void;
}) {
  const [kind, setKind] = useState<TargetKind>("Deployment");
  const [namespace, setNamespace] = useState(namespaces[0] ?? "");
  const [name, setName] = useState("");
  const [hint, setHint] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const clusterScoped = kind === "Node";
  const command = `kubectl page ${kindPrefix[kind]}/${name === "" ? "<name>" : name}${
    clusterScoped || namespace === "" ? "" : ` -n ${namespace}`
  }${hint === "" ? "" : ` --hint "${hint}"`}`;

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const created = await createPage({
        kind,
        namespace: clusterScoped ? undefined : namespace,
        name: name.trim(),
        hint: hint.trim() === "" ? undefined : hint.trim(),
      });
      onCreated(created);
      setName("");
      setHint("");
      onOpenChange(false);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Page a workload</DialogTitle>
          <DialogDescription>
            Pagerless reads the evidence and proposes a change. It never restarts, scales, or patches the
            cluster; the only deliverable is a pull request.
          </DialogDescription>
        </DialogHeader>

        <form
          className="flex flex-col gap-3"
          onSubmit={(event) => {
            event.preventDefault();
            void submit();
          }}
        >
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
              Kind
              <Select
                value={kind}
                onChange={(event) => {
                  setKind(event.target.value as TargetKind);
                }}
              >
                {kinds.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {candidate}
                  </option>
                ))}
              </Select>
            </label>

            <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
              Namespace
              <Select
                value={namespace}
                disabled={clusterScoped}
                onChange={(event) => {
                  setNamespace(event.target.value);
                }}
              >
                {namespaces.map((candidate) => (
                  <option key={candidate} value={candidate}>
                    {candidate}
                  </option>
                ))}
              </Select>
            </label>
          </div>

          <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
            Name
            <Input
              value={name}
              required
              onChange={(event) => {
                setName(event.target.value);
              }}
              placeholder={kind === "Node" ? "ip-10-0-1-17" : "checkout"}
              className="font-mono text-sm"
            />
          </label>

          <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
            Hint (optional)
            <Textarea
              value={hint}
              onChange={(event) => {
                setHint(event.target.value);
              }}
              placeholder="What changed just before this started?"
              className="text-sm"
            />
          </label>

          <CommandBlock command={command} note="The same request from a terminal. Both create one Page." />

          {error !== null && <Notice tone="error">{error}</Notice>}

          <DialogFooter>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => {
                onOpenChange(false);
              }}
            >
              Cancel
            </Button>
            <Button type="submit" size="sm" disabled={busy || name.trim() === "" || (!clusterScoped && namespace === "")}>
              {busy ? "Opening…" : "Open the Page"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
