import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";

import { ErrorNotice } from "../components/notices";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { listTargets, startPage, type TargetOption } from "../lib/api";

const maxHintLength = 1000;

// targetValue is the option's stable key in the select, and the way back to the option.
function targetValue(target: TargetOption): string {
  return [target.kind, target.namespace, target.name].join("/");
}

export function StartPage() {
  const navigate = useNavigate();
  const [targets, setTargets] = useState<TargetOption[]>([]);
  const [selected, setSelected] = useState("");
  const [hint, setHint] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [existing, setExisting] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    listTargets(controller.signal).then(
      (options) => {
        setTargets(options);
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

  const target = targets.find((option) => targetValue(option) === selected);

  const submit = async () => {
    if (target === undefined) {
      setError("Choose a Target to investigate.");
      return;
    }
    setSubmitting(true);
    setError(null);
    setExisting(null);
    try {
      const answer = await startPage(target, hint.trim());
      if (answer.alreadyActive) {
        setExisting(answer.name);
        return;
      }
      navigate(`/pages/${encodeURIComponent(answer.name)}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex max-w-2xl flex-col gap-4">
      <div>
        <h1 className="text-xl font-semibold tracking-tight text-slate-900">Start Page</h1>
        <p className="text-sm text-slate-500">
          Investigate a Deployment or StatefulSet now, with optional guidance for the agent. A Target that
          already has an Active Page keeps it; the existing Page is shown instead of a second one starting.
        </p>
      </div>

      {error !== null && <ErrorNotice>{error}</ErrorNotice>}
      {existing !== null && (
        <Card>
          <CardHeader>
            <CardTitle>That Target already has an Active Page</CardTitle>
            <CardDescription>No new Page was started.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-slate-800">
            <a
              href={`/pages/${encodeURIComponent(existing)}`}
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
          <CardDescription>The Target whose pods are failing.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-col gap-1">
            <label htmlFor="target" className="text-xs font-medium text-slate-600">
              Target
            </label>
            <Select value={selected} onValueChange={setSelected} disabled={loading}>
              <SelectTrigger id="target">
                <SelectValue placeholder={loading ? "Loading Targets…" : "Choose a Target"} />
              </SelectTrigger>
              <SelectContent>
                {targets.map((option) => (
                  <SelectItem key={targetValue(option)} value={targetValue(option)}>
                    {option.namespace}/{option.name} ({option.kind})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {!loading && targets.length === 0 && (
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
            <Button onClick={() => void submit()} disabled={submitting || target === undefined}>
              {submitting ? "Starting…" : "Start Page"}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
