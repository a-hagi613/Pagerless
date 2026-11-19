import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/native-select";
import { CommandBlock } from "@/components/command-block";
import { Notice } from "@/components/notices";
import { createPage } from "@/lib/api";
import type { TargetKind } from "@/lib/types";

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

// The Portal's equivalent of `kubectl page new <TARGET>`. It opens a Page; it never mutates the cluster.
export function PageResourceDialog({
  open,
  onOpenChange,
  namespaces,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  namespaces: string[];
  onCreated: (name: string, alreadyActive: boolean) => void;
}) {
  const [kind, setKind] = useState<TargetKind>("Deployment");
  const [namespace, setNamespace] = useState("");
  const [name, setName] = useState("");
  const [hint, setHint] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [existing, setExisting] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const clusterScoped = kind === "Node";
  const command = `kubectl page new ${kindPrefix[kind]}/${name === "" ? "<name>" : name}${
    clusterScoped || namespace === "" ? "" : ` -n ${namespace}`
  }${hint === "" ? "" : ` --hint "${hint}"`}`;

  const submit = async () => {
    if (name.trim() === "") {
      setError("Name the Target to investigate.");
      return;
    }
    if (!clusterScoped && namespace.trim() === "") {
      setError("A namespaced Target needs a namespace.");
      return;
    }
    setBusy(true);
    setError(null);
    setExisting(null);
    try {
      const answer = await createPage({
        kind,
        namespace: clusterScoped ? undefined : namespace.trim(),
        name: name.trim(),
        hint: hint.trim() === "" ? undefined : hint.trim(),
      });
      onCreated(answer.name, answer.alreadyActive);
      if (answer.alreadyActive) {
        setExisting(answer.name);
        return;
      }
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
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
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
              <Input
                value={clusterScoped ? "" : namespace}
                disabled={clusterScoped}
                list="page-namespaces"
                placeholder={clusterScoped ? "cluster-scoped" : "shop"}
                onChange={(event) => {
                  setNamespace(event.target.value);
                }}
                className="font-mono text-sm"
              />
              <datalist id="page-namespaces">
                {namespaces.map((candidate) => (
                  <option key={candidate} value={candidate} />
                ))}
              </datalist>
            </label>

            <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
              Name
              <Input
                value={name}
                placeholder="checkout"
                onChange={(event) => {
                  setName(event.target.value);
                }}
                className="font-mono text-sm"
              />
            </label>
          </div>

          <label className="flex flex-col gap-1 text-sm font-medium text-foreground">
            Hint (optional)
            <Textarea
              value={hint}
              rows={3}
              maxLength={1000}
              placeholder="What changed, what to look at, or what you have already ruled out."
              onChange={(event) => {
                setHint(event.target.value);
              }}
              className="font-mono text-sm"
            />
          </label>

          {error !== null && <Notice tone="error">{error}</Notice>}
          {existing !== null && (
            <Notice tone="info">
              That Target already has an Active Page. No second Page was started; the existing Page is
              expanded in the inbox.
            </Notice>
          )}

          <CommandBlock command={command} note="The same Page, opened from a terminal." />

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? "Opening…" : "Open the Page"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
