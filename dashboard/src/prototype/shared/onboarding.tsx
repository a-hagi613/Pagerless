import { useState, type ReactNode } from "react";
import {
  Check,
  CircleCheck,
  Cloud,
  Copy,
  Database,
  GitBranch,
  KeyRound,
  Link2,
  Lock,
  Sparkles,
  Terminal,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

import { WEBHOOK_URL } from "../data";
import { usePrototype } from "../store-context";
import { useCopy } from "../use-copy";

function Step({
  index,
  title,
  description,
  done,
  children,
}: {
  index: number;
  title: string;
  description: string;
  done: boolean;
  children: ReactNode;
}) {
  return (
    <section className="rounded-xl border border-border-subtle bg-card">
      <header className="flex flex-wrap items-start gap-3 border-b border-border-subtle px-4 py-3">
        <span
          className={cn(
            "grid size-7 shrink-0 place-items-center rounded-full border font-mono text-[12px]",
            done ? "border-primary/50 bg-primary/15 text-primary" : "border-border text-muted-foreground",
          )}
        >
          {done ? <Check className="size-3.5" aria-hidden="true" /> : index}
        </span>
        <div className="min-w-0 flex-1">
          <h3 className="text-[13px] font-semibold text-foreground">{title}</h3>
          <p className="mt-0.5 text-[12px] leading-relaxed text-muted-foreground">{description}</p>
        </div>
        <Badge variant={done ? "success" : "muted"}>{done ? "configured" : "needs attention"}</Badge>
      </header>
      <div className="flex flex-col gap-3 px-4 py-3">{children}</div>
    </section>
  );
}

function SecretRow({
  icon,
  label,
  value,
  meta,
  action,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  meta: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border-subtle bg-background/40 px-3 py-2">
      <span className="text-muted-foreground">{icon}</span>
      <div className="min-w-0 flex-1">
        <p className="text-[12px] font-medium text-foreground">{label}</p>
        <p className="mt-0.5 truncate font-mono text-[12px] text-muted-foreground">{meta}</p>
      </div>
      <code className="rounded border border-border-subtle bg-muted px-1.5 py-0.5 font-mono text-[12px] text-mint">
        {value}
      </code>
      {action}
    </div>
  );
}

function ConnectorRow({
  icon,
  name,
  detail,
  enabled,
  locked = false,
  onToggle,
}: {
  icon: ReactNode;
  name: string;
  detail: string;
  enabled: boolean;
  locked?: boolean;
  onToggle?: (next: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-lg border border-border-subtle bg-background/40 px-3 py-2">
      <span className="text-muted-foreground">{icon}</span>
      <div className="min-w-0 flex-1">
        <p className="text-[12px] font-medium text-foreground">{name}</p>
        <p className="mt-0.5 text-[12px] leading-snug text-muted-foreground">{detail}</p>
      </div>
      {locked ? (
        <Badge variant="mint">
          <Lock className="size-3" aria-hidden="true" /> always on
        </Badge>
      ) : (
        <Switch
          checked={enabled}
          onCheckedChange={(next) => {
            onToggle?.(next);
          }}
          aria-label={`${name} connector`}
        />
      )}
    </div>
  );
}

// The zero-terminal setup path: AI endpoint, GitHub repository, connectors.
// The same three sections render inside every variant; only the chrome differs.
export function OnboardingSections({ className }: { className?: string }) {
  const { settings, updateSettings, notify } = usePrototype();
  const { copied, copy } = useCopy();
  const [repoOwner, setRepoOwner] = useState("a-hagi613");
  const [repoName, setRepoName] = useState("pagerless-sandbox");

  const connectorsReady =
    Number(settings.lokiConnected) + Number(settings.prometheusConnected) + Number(settings.cloudwatchConnected) + Number(settings.datadogConnected) > 0;

  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-wrap items-center gap-2 rounded-xl border border-primary/25 bg-primary/5 px-4 py-3">
        <Sparkles className="size-4 text-primary" aria-hidden="true" />
        <p className="min-w-0 flex-1 text-[12px] leading-relaxed text-muted-foreground">
          Three steps, no terminal. The CLI performs the same operations, so nothing here is a portal-only shortcut.
        </p>
        <Badge variant="mint">3 of 3 ready</Badge>
      </div>

      <Step
        index={1}
        title="AI endpoint"
        description="Any OpenAI-compatible endpoint. The key is written to the pagerless-ai Secret, never to the browser."
        done
      >
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_auto]">
          <div className="flex flex-col gap-1.5">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="ai-base-url">
              Base URL
            </label>
            <Input
              id="ai-base-url"
              value={settings.aiBaseURL}
              onChange={(event) => {
                updateSettings({ aiBaseURL: event.target.value });
              }}
              className="font-mono text-[12px]"
            />
          </div>
          <div className="flex flex-col gap-1.5 sm:w-52">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="ai-model">
              Model
            </label>
            <Select
              value={settings.aiModel}
              onValueChange={(value) => {
                updateSettings({ aiModel: value });
              }}
            >
              <SelectTrigger id="ai-model" className="font-mono text-[12px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="qwen3-coder-30b">qwen3-coder-30b</SelectItem>
                <SelectItem value="llama-3.3-70b">llama-3.3-70b</SelectItem>
                <SelectItem value="gpt-4.1-mini">gpt-4.1-mini</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>

        <SecretRow
          icon={<KeyRound className="size-3.5" aria-hidden="true" />}
          label="API key"
          value="sk-live-••••••7f3a"
          meta="stored as Secret pagerless-ai/api-key · rotated 3d ago"
          action={
            <Button
              variant="outline"
              size="xs"
              onClick={() => {
                notify("API key replaced", "The Secret was updated and the agent reloaded it.", "success");
              }}
            >
              Replace
            </Button>
          }
        />

        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              notify("Endpoint reachable", `${settings.aiModel} answered in 298ms · streaming supported`, "success");
            }}
          >
            <Terminal className="size-3.5" /> Test endpoint
          </Button>
          <span className="font-mono text-[12px] text-muted-foreground">
            pagerless-agent · {settings.aiModel} · 12 tool calls/min
          </span>
        </div>
      </Step>

      <Step
        index={2}
        title="GitHub repository"
        description="Where Proposals land as draft pull requests. A fine-scoped token keeps the blast radius to one repository."
        done
      >
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="repo-owner">
              Owner
            </label>
            <Input
              id="repo-owner"
              value={repoOwner}
              onChange={(event) => {
                setRepoOwner(event.target.value);
              }}
              className="font-mono text-[12px]"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <label className="text-[12px] font-medium text-muted-foreground" htmlFor="repo-name">
              Repository
            </label>
            <Input
              id="repo-name"
              value={repoName}
              onChange={(event) => {
                setRepoName(event.target.value);
              }}
              className="font-mono text-[12px]"
            />
          </div>
        </div>

        <SecretRow
          icon={<GitBranch className="size-3.5" aria-hidden="true" />}
          label="Personal access token"
          value="ghp_••••••4c1e"
          meta="scopes repo, workflow · verified 2h ago"
          action={
            <Button
              variant="outline"
              size="xs"
              onClick={() => {
                notify("Repository verified", `${repoOwner}/${repoName} · push access confirmed`, "success");
              }}
            >
              Verify
            </Button>
          }
        />

        <div className="flex items-center gap-3">
          <Switch
            id="auto-draft"
            checked={settings.autoDraftPR}
            onCheckedChange={(next) => {
              updateSettings({ autoDraftPR: next });
              notify("Auto-draft updated", next ? "A Proposal opens a draft PR as soon as it is verified." : "Proposals wait for an explicit Create PR.", "info");
            }}
            aria-label="Auto-draft pull requests"
          />
          <label htmlFor="auto-draft" className="min-w-0 flex-1 cursor-pointer">
            <span className="block text-[12px] font-medium text-foreground">Auto-draft pull requests</span>
            <span className="block text-[12px] text-muted-foreground">
              Verified Proposals open a draft PR immediately; approval still happens in GitHub.
            </span>
          </label>
        </div>
      </Step>

      <Step
        index={3}
        title="Log and alert connectors"
        description="Native Kubernetes logs are always available. Add Loki, Prometheus or a cloud provider for richer signals."
        done={connectorsReady}
      >
        <ConnectorRow
          icon={<Database className="size-3.5" aria-hidden="true" />}
          name="Kubernetes API"
          detail="Pods, events and logs read directly from the cluster this server runs in."
          enabled
          locked
        />
        <ConnectorRow
          icon={<Terminal className="size-3.5" aria-hidden="true" />}
          name="Grafana Loki"
          detail="LogQL queries and Loki alert rules become Page triggers."
          enabled={settings.lokiConnected}
          onToggle={(next) => {
            updateSettings({ lokiConnected: next });
            notify(next ? "Loki connected" : "Loki disconnected", next ? "4 labels discovered across 3 namespaces." : "Loki signals are paused for new Pages.", next ? "success" : "info");
          }}
        />
        <ConnectorRow
          icon={<Sparkles className="size-3.5" aria-hidden="true" />}
          name="Prometheus"
          detail="PromQL saturation, restart and probe signals."
          enabled={settings.prometheusConnected}
          onToggle={(next) => {
            updateSettings({ prometheusConnected: next });
          }}
        />
        <ConnectorRow
          icon={<Cloud className="size-3.5" aria-hidden="true" />}
          name="CloudWatch"
          detail="Managed-service logs and metrics from an AWS account."
          enabled={settings.cloudwatchConnected}
          onToggle={(next) => {
            updateSettings({ cloudwatchConnected: next });
          }}
        />
        <ConnectorRow
          icon={<Link2 className="size-3.5" aria-hidden="true" />}
          name="Datadog"
          detail="Import monitors as Page triggers alongside Prometheus alerts."
          enabled={settings.datadogConnected}
          onToggle={(next) => {
            updateSettings({ datadogConnected: next });
          }}
        />

        <Separator />

        <div className="flex flex-col gap-1.5">
          <span className="text-[12px] font-medium text-muted-foreground">Alertmanager webhook</span>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 overflow-x-auto rounded-md border border-border bg-background/60 px-2 py-1.5 font-mono text-[12px] whitespace-nowrap text-mint">
              {WEBHOOK_URL}
            </code>
            <Button
              variant="secondary"
              size="sm"
              aria-label="Copy webhook URL"
              onClick={() => {
                void copy(WEBHOOK_URL).then((ok) => {
                  if (ok) {
                    notify("Webhook copied", "Point Alertmanager's webhook_configs at this URL.", "info");
                  }
                });
              }}
            >
              {copied ? <Check className="size-3.5 text-primary" /> : <Copy className="size-3.5" />}
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <p className="flex items-center gap-1.5 text-[12px] text-muted-foreground">
            <CircleCheck className="size-3 text-primary" aria-hidden="true" />
            Last delivery 14:52 · Alertmanager fired an alert that opened pg-19b2.
          </p>
        </div>
      </Step>
    </div>
  );
}
