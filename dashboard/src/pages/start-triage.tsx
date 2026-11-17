import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";

import { ErrorNotice } from "../components/notices";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { listWorkloads, startTriage, type WorkloadOption } from "../lib/api";

const maxHintLength = 1000;

// workloadValue is the option's stable key in the select, and the way back to the option.
function workloadValue(workload: WorkloadOption): string {
  return [workload.kind, workload.namespace, workload.name].join("/");
}

export function StartTriagePage() {
  const navigate = useNavigate();
  const [workloads, setWorkloads] = useState<WorkloadOption[]>([]);
  const [selected, setSelected] = useState("");
  const [hint, setHint] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [existing, setExisting] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    listWorkloads(controller.signal).then(
      (options) => {
        setWorkloads(options);
        setLoading(false);
      },
      (cause: unknown) => {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        setError(cause instanceof Error ? cause.message : String(cause));
        setLoading(false);
      },
    );
    return () => {
      controller.abort();
    };
  }, []);

  const workload = workloads.find((option) => workloadValue(option) === selected);

  const submit = async () => {
    if (workload === undefined) {
      setError("Choose a Workload to investigate.");
      return;
    }
    setSubmitting(true);
    setError(null);
    setExisting(null);
    try {
      const answer = await startTriage(workload, hint.trim());
      if (answer.alreadyActive) {
        setExisting(answer.name);
        return;
      }
      navigate(`/triages/${encodeURIComponent(answer.name)}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex max-w-2xl flex-col gap-4">
      <div>
        <h1 className="text-xl font-semibold tracking-tight text-slate-900">Start Triage</h1>
        <p className="text-sm text-slate-500">
          Investigate a Deployment or StatefulSet now, with optional guidance for the agent. A Workload that
          already has an Active Triage keeps it; the existing Triage is shown instead of a second one starting.
        </p>
      </div>

      {error !== null && <ErrorNotice>{error}</ErrorNotice>}
      {existing !== null && (
        <Card>
          <CardHeader>
            <CardTitle>That Workload already has an Active Triage</CardTitle>
            <CardDescription>No new Triage was started.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-slate-800">
            <a
              href={`/triages/${encodeURIComponent(existing)}`}
              className="font-mono text-sky-700 hover:underline"
            >
              {existing}
            </a>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>What to investigate</CardTitle>
          <CardDescription>The Workload whose pods are failing.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-col gap-1">
            <label htmlFor="workload" className="text-xs font-medium text-slate-600">
              Workload
            </label>
            <Select value={selected} onValueChange={setSelected} disabled={loading}>
              <SelectTrigger id="workload">
                <SelectValue placeholder={loading ? "Loading Workloads…" : "Choose a Workload"} />
              </SelectTrigger>
              <SelectContent>
                {workloads.map((option) => (
                  <SelectItem key={workloadValue(option)} value={workloadValue(option)}>
                    {option.namespace}/{option.name} ({option.kind})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {!loading && workloads.length === 0 && (
              <p className="text-xs text-slate-500">
                No Deployments or StatefulSets are available to the dashboard.
              </p>
            )}
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="hint" className="text-xs font-medium text-slate-600">
              Hint (optional)
            </label>
            <textarea
              id="hint"
              value={hint}
              maxLength={maxHintLength}
              rows={4}
              onChange={(event) => {
                setHint(event.target.value);
              }}
              placeholder="What changed, what to look at, or what you have already ruled out."
              className="w-full rounded-md border border-slate-300 bg-white px-2.5 py-2 text-sm text-slate-900 shadow-xs placeholder:text-slate-400"
            />
            <p className="text-right text-xs text-slate-500">
              {hint.length}/{maxHintLength}
            </p>
          </div>

          <div className="flex items-center gap-2">
            <Button onClick={() => void submit()} disabled={submitting || workload === undefined}>
              {submitting ? "Starting…" : "Start Triage"}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
