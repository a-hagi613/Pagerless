import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, ExternalLink } from "lucide-react";

import { Age } from "../components/age";
import { ErrorNotice } from "../components/notices";
import { PhaseBadge } from "../components/phase-badge";
import { TriggerLabel } from "../components/trigger-label";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import {
  decideTriage,
  eventsURL,
  getTriage,
  type DashboardLink,
  type FindingKind,
  type GrafanaLinks,
  type Step,
  type TriageDetail,
} from "../lib/api";
import { useSession } from "../lib/use-session";
import { formatTime } from "../lib/time";

type StreamState = "connecting" | "live" | "reconnecting" | "closed";

// stepKey identifies a step across the snapshot and the live stream, so a step is never shown twice.
function stepKey(step: Step): string {
  return [step.time, step.kind, step.tool ?? "", step.summary].join("|");
}

function sourceLink(source: string) {
  if (/^https?:\/\//u.test(source)) {
    return (
      <a
        href={source}
        target="_blank"
        rel="noreferrer"
        className="inline-flex items-center gap-1 break-all text-sky-700 hover:underline"
      >
        {source}
        <ExternalLink aria-hidden="true" className="size-3" />
      </a>
    );
  }
  return <span className="break-all font-mono text-xs text-slate-600">{source}</span>;
}

function streamLabel(state: StreamState): string {
  switch (state) {
    case "connecting":
      return "Connecting to live steps";
    case "live":
      return "Live";
    case "reconnecting":
      return "Reconnecting";
    case "closed":
      return "Stream closed";
  }
}

export function TriageDetailPage() {
  const { name = "" } = useParams();
  const session = useSession();
  const [record, setRecord] = useState<TriageDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [stream, setStream] = useState<StreamState>("connecting");
  const [reason, setReason] = useState("");
  const [deciding, setDeciding] = useState(false);
  const [decisionError, setDecisionError] = useState<string | null>(null);

  const refresh = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const next = await getTriage(name, signal);
        setRecord(next);
        setError(null);
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        setError(cause instanceof Error ? cause.message : String(cause));
      }
    },
    [name],
  );

  const decide = useCallback(
    async (outcome: "Approved" | "Rejected") => {
      setDeciding(true);
      setDecisionError(null);
      try {
        await decideTriage(name, outcome, reason.trim());
        setReason("");
        await refresh();
      } catch (cause) {
        setDecisionError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setDeciding(false);
      }
    },
    [name, reason, refresh],
  );

  useEffect(() => {
    const controller = new AbortController();
    let disposed = false;
    getTriage(name, controller.signal).then(
      (next) => {
        if (!disposed) {
          setRecord(next);
          setError(null);
        }
      },
      (cause: unknown) => {
        if (disposed || (cause instanceof DOMException && cause.name === "AbortError")) {
          return;
        }
        setError(cause instanceof Error ? cause.message : String(cause));
      },
    );
    return () => {
      disposed = true;
      controller.abort();
    };
  }, [name]);

  useEffect(() => {
    if (name === "") {
      return;
    }
    const source = new EventSource(eventsURL(name));
    const onSnapshot = (event: MessageEvent<string>) => {
      setRecord(JSON.parse(event.data) as TriageDetail);
      setStream("live");
    };
    const onStep = (event: MessageEvent<string>) => {
      const data = JSON.parse(event.data) as {
        step: Step;
        finding?: TriageDetail["findings"][number];
        stepCount: number;
      };
      const incoming = data.finding;
      setRecord((current) => {
        if (current === null) {
          return current;
        }
        const known = current.steps.some((step) => stepKey(step) === stepKey(data.step));
        const findings =
          incoming === undefined ||
          current.findings.some(
            (finding) =>
              finding.kind === incoming.kind &&
              finding.source === incoming.source &&
              finding.summary === incoming.summary,
          )
            ? current.findings
            : [...current.findings, incoming];
        return {
          ...current,
          findings,
          stepCount: data.stepCount,
          steps: known ? current.steps : [...current.steps, data.step],
        };
      });
    };
    const onPhase = (event: MessageEvent<string>) => {
      const data = JSON.parse(event.data) as { phase: TriageDetail["phase"] };
      setRecord((current) => (current === null ? current : { ...current, phase: data.phase }));
      void refresh();
    };
    const onEnd = () => {
      setStream("closed");
      source.close();
    };
    const onError = () => {
      setStream("reconnecting");
    };
    source.addEventListener("snapshot", onSnapshot);
    source.addEventListener("step", onStep);
    source.addEventListener("phase", onPhase);
    source.addEventListener("end", onEnd);
    source.addEventListener("error", onError);
    return () => {
      source.close();
    };
  }, [name, refresh]);

  if (error !== null && record === null) {
    return (
      <div className="flex flex-col gap-4">
        <BackLink />
        <ErrorNotice>{error}</ErrorNotice>
      </div>
    );
  }
  if (record === null) {
    return (
      <div className="flex flex-col gap-4">
        <BackLink />
        <p className="text-sm text-slate-500">Loading the Triage record…</p>
      </div>
    );
  }

  const { proposal, verification, grafana } = record;

  return (
    <div className="flex flex-col gap-4">
      <BackLink />

      {error !== null && <ErrorNotice>{error}</ErrorNotice>}

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight text-slate-900">
              {record.workload.namespace}/{record.workload.name}
            </h1>
            <PhaseBadge phase={record.phase} />
            {record.source === "history" && (
              <Badge variant="muted" title="The Triage resource was deleted after retention">
                from history
              </Badge>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-slate-500">
            <span className="font-mono">{record.name}</span>
            <span>{record.workload.kind}</span>
            <span>
              started <Age at={record.createdAt} /> ago
            </span>
            {record.terminalAt !== undefined && <span>ended {formatTime(record.terminalAt)}</span>}
            <span>{String(record.triggerCount)} Trigger(s)</span>
            <span>{String(record.stepCount)} step(s)</span>
            {record.toolCallCount > 0 && <span>{String(record.toolCallCount)} tool call(s)</span>}
            {record.usage !== undefined && (
              <span title="Estimated model cost">
                ${record.usage.costUSD.toFixed(4)} · {String(record.usage.inputTokens)} in /{" "}
                {String(record.usage.outputTokens)} out
              </span>
            )}
          </div>
        </div>
        <div className="flex flex-col items-end gap-1 text-xs">
          <TriggerLabel trigger={record.trigger} />
          <span
            aria-live="polite"
            className={
              stream === "live"
                ? "text-emerald-700"
                : stream === "reconnecting"
                  ? "text-amber-700"
                  : "text-slate-500"
            }
          >
            {streamLabel(stream)}
          </span>
        </div>
      </div>

      {record.hint !== undefined && record.hint !== "" && (
        <Card>
          <CardHeader>
            <CardTitle>Hint</CardTitle>
            <CardDescription>Guidance the human attached to the Trigger.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm whitespace-pre-wrap text-slate-800">{record.hint}</CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Live steps</CardTitle>
          <CardDescription>
            Step events as pagerless-server records them; status keeps the most recent steps and history keeps
            every one.
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {record.steps.length === 0 ? (
            <p className="px-4 py-3 text-sm text-slate-500">No steps have been reported yet.</p>
          ) : (
            <ol
              className="max-h-[28rem] divide-y divide-slate-100 overflow-y-auto"
              aria-live="polite"
              data-testid="steps"
            >
              {record.steps.map((step, index) => (
                <li
                  key={`${String(index)}-${stepKey(step)}`}
                  className="flex gap-3 px-4 py-2 text-sm"
                  data-testid="step"
                >
                  <span className="w-20 shrink-0 font-mono text-xs text-slate-500" title={formatTime(step.time)}>
                    {new Date(step.time).toLocaleTimeString(undefined, { hour12: false })}
                  </span>
                  <Badge variant="muted" className="h-fit shrink-0">
                    {step.kind}
                  </Badge>
                  <span className="min-w-0 flex-1 text-slate-800">
                    {step.tool !== undefined && (
                      <span className="mr-1.5 font-mono text-xs text-slate-500">{step.tool}</span>
                    )}
                    {step.summary}
                  </span>
                </li>
              ))}
            </ol>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Findings</CardTitle>
          <CardDescription>The evidence the Diagnosis cites.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {record.findings.length === 0 ? (
            <p className="text-sm text-slate-500">The agent has not cited a Finding yet.</p>
          ) : (
            record.findings.map((finding, index) => (
              <div key={`${String(index)}-${finding.source}`} className="flex flex-col gap-1">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant="outline">{finding.kind}</Badge>
                  {sourceLink(finding.source)}
                  <FindingTelemetryLink grafana={record.grafana} kind={finding.kind} />
                </div>
                <p className="text-sm text-slate-800">{finding.summary}</p>
              </div>
            ))
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Diagnosis</CardTitle>
        </CardHeader>
        <CardContent>
          {record.diagnosis === undefined || record.diagnosis === "" ? (
            <p className="text-sm text-slate-500">The agent has not reported a Diagnosis yet.</p>
          ) : (
            <p className="text-sm leading-relaxed whitespace-pre-wrap text-slate-800">{record.diagnosis}</p>
          )}
        </CardContent>
      </Card>

      {record.decision !== undefined && (
        <Card>
          <CardHeader>
            <CardTitle>Decision</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
            <Badge variant={record.decision.outcome === "Approved" ? "success" : "danger"}>
              {record.decision.outcome}
            </Badge>
            <span className="text-slate-700" data-testid="decision-approver">
              {record.decision.approver}
            </span>
            <span className="text-xs text-slate-500">{formatTime(record.decision.decidedAt)}</span>
            {record.decision.reason !== undefined && record.decision.reason !== "" && (
              <span className="basis-full text-slate-700">{record.decision.reason}</span>
            )}
          </CardContent>
        </Card>
      )}

      {record.phase === "Proposed" && proposal !== undefined && (
        <Card>
          <CardHeader>
            <CardTitle>Decision</CardTitle>
            <CardDescription>
              Approving binds exactly this Proposal, identified by its hash. If it changes before the decision is
              admitted, the decision is refused and the new Proposal must be reviewed.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {decisionError !== null && <ErrorNotice>{decisionError}</ErrorNotice>}
            {session.canApprove ? (
              <>
                <label htmlFor="decision-reason" className="text-xs font-medium text-slate-600">
                  Reason (optional)
                </label>
                <textarea
                  id="decision-reason"
                  rows={2}
                  value={reason}
                  maxLength={1000}
                  onChange={(event) => {
                    setReason(event.target.value);
                  }}
                  placeholder="Why this decision is right."
                  className="w-full rounded-md border border-slate-300 bg-white px-2.5 py-2 text-sm text-slate-900 shadow-xs placeholder:text-slate-400"
                />
                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    data-testid="approve"
                    disabled={deciding}
                    onClick={() => void decide("Approved")}
                  >
                    {proposal.action === "NoAction" ? "Acknowledge Diagnosis" : "Approve"}
                  </Button>
                  <Button
                    variant="outline"
                    disabled={deciding}
                    onClick={() => void decide("Rejected")}
                  >
                    Reject
                  </Button>
                  <span className="text-xs text-slate-500">
                    Recorded as {session.username}, verified through Keycloak.
                  </span>
                </div>
              </>
            ) : (
              <p className="text-sm text-slate-600">
                Your account is not a member of the Keycloak approvers group, so it can read this Triage but not
                decide it.
              </p>
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Proposal</CardTitle>
          <CardDescription>The typed remediation that Validation accepted.</CardDescription>
        </CardHeader>
        <CardContent>
          {proposal === undefined ? (
            <p className="text-sm text-slate-500">The agent has not proposed a remediation yet.</p>
          ) : (
            <div className="flex flex-col gap-4">
              <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-[9rem_1fr]">
                <dt className="text-slate-500">Action</dt>
                <dd>
                  <Badge variant="info">{proposal.action}</Badge>
                </dd>
                <dt className="text-slate-500">Attempt</dt>
                <dd className="text-slate-800">{String(proposal.attempt)}</dd>
                <dt className="text-slate-500">Hash</dt>
                <dd className="break-all font-mono text-xs text-slate-700">{proposal.hash}</dd>
                <dt className="text-slate-500">Expires</dt>
                <dd className="text-slate-800">{formatTime(proposal.expiresAt)}</dd>
                {proposal.base?.commit !== undefined && (
                  <>
                    <dt className="text-slate-500">Base commit</dt>
                    <dd className="break-all font-mono text-xs text-slate-700">{proposal.base.commit}</dd>
                  </>
                )}
              </dl>
              {proposal.parameters !== undefined && (
                <div>
                  <h3 className="mb-1 text-xs font-semibold tracking-wide text-slate-600 uppercase">
                    Parameters
                  </h3>
                  <pre className="overflow-x-auto rounded-md border border-slate-200 bg-slate-50 p-3 font-mono text-xs text-slate-800">
                    {JSON.stringify(proposal.parameters, null, 2)}
                  </pre>
                </div>
              )}
              <div>
                <h3 className="mb-1 text-xs font-semibold tracking-wide text-slate-600 uppercase">
                  Rendered change
                </h3>
                {proposal.renderedChange === undefined ? (
                  <p className="text-sm text-slate-500">This Action renders no change.</p>
                ) : (
                  <div className="flex flex-col gap-2">
                    {proposal.renderedChange.path !== undefined && (
                      <p className="font-mono text-xs text-slate-600">{proposal.renderedChange.path}</p>
                    )}
                    {proposal.renderedChange.patch !== undefined ? (
                      <pre className="max-h-[24rem] overflow-auto rounded-md border border-slate-200 bg-slate-950 p-3 font-mono text-xs leading-relaxed text-slate-100">
                        {proposal.renderedChange.patch}
                      </pre>
                    ) : (
                      <p className="text-sm text-slate-500">Validation rendered no patch for this Action.</p>
                    )}
                  </div>
                )}
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Execution</CardTitle>
          <CardDescription>What the Executor carried out.</CardDescription>
        </CardHeader>
        <CardContent className="text-sm">
          {record.execution === undefined ? (
            <p className="text-slate-500">The Proposal has not been executed.</p>
          ) : (
            <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-[9rem_1fr]">
              {record.execution.pullRequestURL !== undefined && (
                <>
                  <dt className="text-slate-500">Pull request</dt>
                  <dd>
                    <a
                      href={record.execution.pullRequestURL}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex items-center gap-1 break-all text-sky-700 hover:underline"
                    >
                      {record.execution.pullRequestURL}
                      <ExternalLink aria-hidden="true" className="size-3" />
                    </a>
                  </dd>
                </>
              )}
              {record.execution.mergeCommit !== undefined && (
                <>
                  <dt className="text-slate-500">Merge commit</dt>
                  <dd className="break-all font-mono text-xs text-slate-700">
                    {record.execution.mergeCommit}
                  </dd>
                </>
              )}
              {record.execution.restartedPods !== undefined &&
                record.execution.restartedPods.length > 0 && (
                  <>
                    <dt className="text-slate-500">Restarted pods</dt>
                    <dd className="flex flex-col gap-1">
                      {record.execution.restartedPods.map((pod) => (
                        <span key={pod} className="font-mono text-xs text-slate-700">
                          {pod}
                        </span>
                      ))}
                    </dd>
                  </>
                )}
              {record.execution.executedAt !== undefined && (
                <>
                  <dt className="text-slate-500">Executed</dt>
                  <dd className="text-slate-800">{formatTime(record.execution.executedAt)}</dd>
                </>
              )}
            </dl>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Verification</CardTitle>
          <CardDescription>The deterministic checks after execution.</CardDescription>
        </CardHeader>
        <CardContent className="text-sm">
          {verification === undefined ? (
            <p className="text-slate-500">Verification has not started.</p>
          ) : (
            <div className="flex flex-col gap-3">
              {verification.startedAt !== undefined && (
                <p className="text-xs text-slate-500">
                  Observation window began {formatTime(verification.startedAt)}
                </p>
              )}
              {verification.checks.length === 0 ? (
                <p className="text-slate-500">Verification is still observing the Workload.</p>
              ) : (
                <ol className="flex flex-col gap-2">
                  {verification.checks.map((check) => (
                    <li key={check.name} className="flex flex-wrap items-start gap-2">
                      <Badge variant={check.passed ? "success" : "danger"}>
                        {check.passed ? "pass" : "fail"}
                      </Badge>
                      <div className="min-w-0 flex-1">
                        <p className="font-medium text-slate-800">{check.name}</p>
                        {check.message !== undefined && (
                          <p className="text-xs text-slate-600">{check.message}</p>
                        )}
                      </div>
                    </li>
                  ))}
                </ol>
              )}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Narrative</CardTitle>
          <CardDescription>
            A descriptive account written after Verification. It never affects the outcome.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {record.narrative === undefined || record.narrative === "" ? (
            <p className="text-sm text-slate-500">No Narrative has been written yet.</p>
          ) : (
            <p className="text-sm leading-relaxed whitespace-pre-wrap text-slate-800">{record.narrative}</p>
          )}
        </CardContent>
      </Card>

      {grafana !== undefined && (
        <section aria-labelledby="grafana-heading" className="flex flex-col gap-3">
          <div>
            <h2 id="grafana-heading" className="text-base font-semibold tracking-tight text-slate-900">
              Workload telemetry
            </h2>
            <p className="text-sm text-slate-500">
              Metrics and logs for {record.workload.namespace}/{record.workload.name}, from Grafana. If a panel
              does not load, open it directly in Grafana.
            </p>
          </div>
          <div className="grid gap-4 xl:grid-cols-2">
            {grafana.metrics !== undefined && (
              <GrafanaPanel
                title="Metrics"
                description="CPU, memory, restarts and ready replicas for the Workload."
                link={grafana.metrics}
              />
            )}
            {grafana.logs !== undefined && (
              <GrafanaPanel
                title="Logs"
                description="Container logs collected by the OpenTelemetry Collector."
                link={grafana.logs}
              />
            )}
          </div>
        </section>
      )}
    </div>
  );
}

// FindingTelemetryLink opens the Workload's Grafana view that matches a Finding's evidence, so every Finding has
// a source the reader can inspect even when its citation is not a URL.
function FindingTelemetryLink({
  grafana,
  kind,
}: {
  grafana: GrafanaLinks | undefined;
  kind: FindingKind;
}) {
  const link = kind === "Log" ? grafana?.logs : kind === "Metric" ? grafana?.metrics : undefined;
  if (link === undefined) {
    return null;
  }
  return (
    <a
      href={link.deepLink}
      target="_blank"
      rel="noreferrer"
      className="inline-flex items-center gap-1 text-xs text-sky-700 hover:underline"
    >
      Open {kind.toLowerCase()} in Grafana
      <ExternalLink aria-hidden="true" className="size-3" />
    </a>
  );
}

function GrafanaPanel({
  title,
  description,
  link,
}: {
  title: string;
  description: string;
  link: DashboardLink;
}) {
  return (
    <Card className="overflow-hidden">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle>{title}</CardTitle>
            <CardDescription>{description}</CardDescription>
          </div>
          <a
            href={link.deepLink}
            target="_blank"
            rel="noreferrer"
            className="inline-flex shrink-0 items-center gap-1 text-xs text-sky-700 hover:underline"
          >
            Open in Grafana
            <ExternalLink aria-hidden="true" className="size-3" />
          </a>
        </div>
      </CardHeader>
      <iframe
        src={link.embedURL}
        title={`${title} for the Workload in Grafana`}
        className="h-96 w-full border-0 bg-white"
        loading="lazy"
        referrerPolicy="strict-origin-when-cross-origin"
      />
    </Card>
  );
}

function BackLink() {
  return (
    <Link to="/" className="inline-flex w-fit items-center gap-1 text-sm text-sky-700 hover:underline">
      <ArrowLeft aria-hidden="true" className="size-4" /> Triages
    </Link>
  );
}
