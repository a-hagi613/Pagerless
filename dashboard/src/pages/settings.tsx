import { Check, Copy, KeyRound, Lock, Server, ShieldCheck, Terminal } from "lucide-react";
import { useEffect, useMemo, useState } from "react";

import { AppHeader } from "@/components/app-header";
import { CliDrawer } from "@/components/cli-drawer";
import { CommandBlock } from "@/components/command-block";
import { Notice } from "@/components/notices";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { getSettings, updateSettings, testSettings, UnauthenticatedError, type TestTarget } from "@/lib/api";
import { signOut } from "@/lib/sign-out";
import { shortHash } from "@/lib/format";
import type { Connector, ConnectorName, MaskedValue, Settings } from "@/lib/types";
import { useCopy } from "@/lib/use-copy";
import { useSession } from "@/lib/use-session";

type Tone = "info" | "success" | "error";
type Feedback = { tone: Tone; text: string };

const connectorLabels: Record<ConnectorName, string> = {
  native_k8s_logs: "Kubernetes API logs",
  loki: "Grafana Loki",
  prometheus: "Prometheus",
  cloudwatch: "CloudWatch",
  datadog: "Datadog",
};

function MaskedChip({ value }: { value: MaskedValue }) {
  if (!value.configured) {
    return <Badge variant="muted">not configured</Badge>;
  }
  return (
    <Badge variant="mint" className="font-mono">
      sha256:{shortHash(value.fingerprint)}
    </Badge>
  );
}

// A write-only credential field: the input is empty by design, and the fingerprint beside it is the
// only representation of the stored value that ever reaches the browser.
function CredentialField({
  id,
  label,
  value,
  masked,
  readOnly,
  placeholder,
  testId,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  masked: MaskedValue;
  readOnly: boolean;
  placeholder: string;
  testId?: string;
  onChange: (next: string) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-2">
      <label className="flex min-w-0 flex-1 flex-col gap-1 text-sm font-medium text-foreground" htmlFor={id}>
        {label}
        <Input
          id={id}
          data-testid={testId}
          value={value}
          readOnly={readOnly}
          type="password"
          autoComplete="off"
          onChange={(event) => {
            onChange(event.target.value);
          }}
          className="font-mono text-sm"
          placeholder={placeholder}
        />
      </label>
      <div className="flex flex-col items-start gap-1">
        <span className="text-xs text-muted-foreground">Current fingerprint</span>
        <MaskedChip value={masked} />
      </div>
    </div>
  );
}

function ConnectorRow({
  connector,
  readOnly,
  busy,
  onSave,
  onTest,
  testResult,
}: {
  connector: Connector;
  readOnly: boolean;
  busy: boolean;
  onSave: (name: ConnectorName, patch: { enabled: boolean; url: string; token?: string }) => void;
  onTest: (target: TestTarget) => void;
  testResult?: Feedback;
}) {
  const [enabled, setEnabled] = useState(connector.enabled);
  const [url, setUrl] = useState(connector.url);
  const [token, setToken] = useState("");

  const locked = connector.locked === true;
  const inputId = `connector-${connector.name}`;

  return (
    <div className="flex flex-col gap-2 py-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 flex-1">
          <span className="block text-sm font-medium text-foreground">{connectorLabels[connector.name]}</span>
          <span className="block font-mono text-xs text-muted-foreground">
            {connector.name} · {connector.type} · {connector.status}
          </span>
        </span>
        <MaskedChip value={connector.token} />
        {locked ? (
          <Badge variant="mint">
            <Lock className="size-3" aria-hidden="true" /> always on
          </Badge>
        ) : (
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            Enabled
            <Switch
              checked={enabled}
              disabled={readOnly}
              onCheckedChange={setEnabled}
              aria-label={`Enable the ${connectorLabels[connector.name]} connector`}
            />
          </label>
        )}
      </div>

      {!locked && (
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground" htmlFor={`${inputId}-url`}>
            Endpoint
            <Input
              id={`${inputId}-url`}
              value={url}
              readOnly={readOnly}
              onChange={(event) => {
                setUrl(event.target.value);
              }}
              className="font-mono text-xs"
              placeholder="https://loki.monitoring.svc:3100"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground" htmlFor={`${inputId}-token`}>
            Token (write-only)
            <Input
              id={`${inputId}-token`}
              value={token}
              readOnly={readOnly}
              type="password"
              autoComplete="off"
              onChange={(event) => {
                setToken(event.target.value);
              }}
              className="font-mono text-xs"
              placeholder={connector.token.configured ? "configured · submit to rotate" : "not configured"}
            />
          </label>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {!locked && (
          <Button
            size="xs"
            variant="secondary"
            disabled={readOnly || busy}
            onClick={() => {
              onSave(connector.name, {
                enabled,
                url: url.trim(),
                ...(token === "" ? {} : { token }),
              });
            }}
          >
            Save connector
          </Button>
        )}
        <Button
          size="xs"
          variant="outline"
          disabled={busy}
          onClick={() => {
            onTest(`connector:${connector.name}`);
          }}
        >
          <Terminal className="size-3" aria-hidden="true" /> Test connection
        </Button>
        {testResult !== undefined && (
          <span
            role="status"
            className={testResult.tone === "error" ? "text-xs text-destructive" : "text-xs text-muted-foreground"}
          >
            {testResult.text}
          </span>
        )}
      </div>
    </div>
  );
}

export function SettingsPage() {
  const { session, refresh: refreshSession } = useSession();
  const [settings, setSettings] = useState<Settings | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [feedback, setFeedback] = useState<Record<string, Feedback>>({});
  const [cliOpen, setCliOpen] = useState(false);
  const { copied, copy } = useCopy();

  // AI endpoint and Target Repository drafts.
  const [baseUrl, setBaseUrl] = useState("");
  const [model, setModel] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [owner, setOwner] = useState("");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState("");
  const [autoDraft, setAutoDraft] = useState(false);
  const [githubToken, setGithubToken] = useState("");

  const readOnly = settings?.managedBy === "external" || settings?.writeCredentials === false;

  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    getSettings(controller.signal)
      .then((value) => {
        if (!active) {
          return;
        }
        setSettings(value);
        setBaseUrl(value.ai.baseUrl);
        setModel(value.ai.model);
        setOwner(value.repository.owner);
        setRepo(value.repository.repo);
        setBranch(value.repository.branch);
        setAutoDraft(value.repository.autoDraftPR);
        setLoadError(null);
      })
      .catch((cause: unknown) => {
        if (!active || (cause instanceof DOMException && cause.name === "AbortError")) {
          return;
        }
        if (cause instanceof UnauthenticatedError) {
          void refreshSession();
          return;
        }
        setLoadError(cause instanceof Error ? cause.message : String(cause));
      })
      .finally(() => {
        if (active) {
          setLoading(false);
        }
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [refreshSession]);

  const say = (key: string, tone: Tone, text: string) => {
    setFeedback((current) => ({ ...current, [key]: { tone, text } }));
  };

  const save = async (key: string, patch: Parameters<typeof updateSettings>[0]) => {
    setBusy(key);
    setFeedback((current) => ({ ...current, [key]: { tone: "info", text: "" } }));
    try {
      const next = await updateSettings(patch);
      setSettings(next);
      setApiKey("");
      setGithubToken("");
      say(key, "success", "Saved. The masked fingerprint is the proof the write landed.");
    } catch (cause) {
      if (cause instanceof UnauthenticatedError) {
        void refreshSession();
        return;
      }
      const message = cause instanceof Error ? cause.message : String(cause);
      if (message.toLowerCase().includes("external") || message.toLowerCase().includes("credential_writes_disabled")) {
        say(key, "info", "These credentials are managed by an external Secret, so the write was refused. The form is read-only.");
        setSettings((current) => (current === null ? current : { ...current, managedBy: "external", writeCredentials: false }));
      } else {
        say(key, "error", message);
      }
    } finally {
      setBusy(null);
    }
  };

  const test = async (key: string, target: TestTarget) => {
    setBusy(key);
    setFeedback((current) => ({ ...current, [key]: { tone: "info", text: "" } }));
    try {
      const result = await testSettings(target);
      setFeedback((current) => ({
        ...current,
        [key]: { tone: result.ok ? "success" : "error", text: result.detail },
      }));
    } catch (cause) {
      if (cause instanceof UnauthenticatedError) {
        void refreshSession();
        return;
      }
      setFeedback((current) => ({
        ...current,
        [key]: { tone: "error", text: cause instanceof Error ? cause.message : String(cause) },
      }));
    } finally {
      setBusy(null);
    }
  };

  const connectors = useMemo(() => settings?.connectors ?? [], [settings]);

  if (session === null) {
    return null;
  }

  return (
    <div className="min-h-svh bg-background pb-10">
      <a
        href="#content"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:text-foreground"
      >
        Skip to content
      </a>
      <AppHeader
        session={session}
        onOpenCli={() => {
          setCliOpen(true);
        }}
        onSignOut={() => {
          void signOut(refreshSession);
        }}
      />

      <main id="content" className="mx-auto flex w-full max-w-[1000px] flex-col gap-4 px-3 py-4 sm:px-5">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">Settings</h1>
          <p className="text-sm text-muted-foreground">
            The AI endpoint, the Target Repository, and the connectors every Page uses. Each control has a{" "}
            <span className="font-mono text-foreground">kubectl page config</span> equivalent.
          </p>
        </div>

        {loadError !== null && <Notice tone="error">Settings could not be read: {loadError}</Notice>}

        {readOnly && (
          <div data-testid="settings-external-banner">
            <Notice tone="info">
              <span className="font-semibold">Managed by an external secret manager.</span> Every field below is
              configured and read-only; writes are refused on both surfaces. Read the values with{" "}
              <span className="font-mono">kubectl page config get</span>, or copy the Secret command below.
            </Notice>
          </div>
        )}

        {loading && settings === null && <p className="text-sm text-muted-foreground">Loading the settings…</p>}

        {settings !== null && (
          <>
            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <Server className="size-4 text-primary" aria-hidden="true" /> AI endpoint
                  <Badge variant={settings.ai.apiKey.configured ? "mint" : "warning"}>
                    {settings.ai.apiKey.configured ? "configured" : "needs a key"}
                  </Badge>
                </CardTitle>
                <CardDescription>
                  Any OpenAI-compatible endpoint: OpenAI, DeepSeek, Ollama, vLLM, or a gateway. The key is
                  written to the credential Secret and never returns to the browser.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_1fr]">
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-ai-base-url">
                    Base URL
                    <Input
                      id="settings-ai-base-url"
                      data-testid="settings-ai-base-url"
                      value={baseUrl}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setBaseUrl(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="https://api.deepseek.com/v1"
                    />
                  </label>
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-ai-model">
                    Model
                    <Input
                      id="settings-ai-model"
                      data-testid="settings-ai-model"
                      value={model}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setModel(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="deepseek-chat"
                    />
                  </label>
                </div>

                <CredentialField
                  id="settings-ai-key"
                  testId="settings-ai-key"
                  label="API key (write-only)"
                  value={apiKey}
                  masked={settings.ai.apiKey}
                  readOnly={readOnly}
                  placeholder={settings.ai.apiKey.configured ? "configured · submit to rotate" : "sk-…"}
                  onChange={setApiKey}
                />

                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    size="sm"
                    data-testid="settings-ai-save"
                    disabled={readOnly || busy === "ai"}
                    onClick={() => {
                      void save("ai", {
                        ai: {
                          baseUrl: baseUrl.trim(),
                          model: model.trim(),
                          ...(apiKey === "" ? {} : { apiKey }),
                        },
                      });
                    }}
                  >
                    <KeyRound className="size-3.5" aria-hidden="true" /> Save AI endpoint
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    data-testid="settings-ai-test"
                    disabled={busy === "ai"}
                    onClick={() => {
                      void test("ai", "ai");
                    }}
                  >
                    Test connection
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={readOnly || settings.ai.apiKey.configured === false}
                    onClick={() => {
                      void save("ai", { ai: { apiKey: "" } });
                    }}
                  >
                    Remove key
                  </Button>
                  {feedback.ai !== undefined && feedback.ai.text !== "" && (
                    <span
                      role="status"
                      className={feedback.ai.tone === "error" ? "text-sm text-destructive" : "text-sm text-muted-foreground"}
                    >
                      {feedback.ai.text}
                    </span>
                  )}
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <ShieldCheck className="size-4 text-primary" aria-hidden="true" /> Target Repository
                  <Badge variant={settings.repository.token.configured ? "mint" : "warning"}>
                    {settings.repository.owner === "" || settings.repository.repo === ""
                      ? "not configured"
                      : `${settings.repository.owner}/${settings.repository.repo}`}
                  </Badge>
                </CardTitle>
                <CardDescription>
                  Where an approved Proposal becomes a draft pull request. The token is scoped to this
                  repository and never leaves the cluster.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-repo-owner">
                    Owner
                    <Input
                      id="settings-repo-owner"
                      data-testid="settings-repo-owner"
                      value={owner}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setOwner(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="a-hagi613"
                    />
                  </label>
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-repo-name">
                    Repository
                    <Input
                      id="settings-repo-name"
                      data-testid="settings-repo-name"
                      value={repo}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setRepo(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="k8s-manifests"
                    />
                  </label>
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-repo-branch">
                    Branch
                    <Input
                      id="settings-repo-branch"
                      value={branch}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setBranch(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="main"
                    />
                  </label>
                </div>

                <CredentialField
                  id="settings-repo-token"
                  label="GitHub token (write-only)"
                  value={githubToken}
                  masked={settings.repository.token}
                  readOnly={readOnly}
                  placeholder={settings.repository.token.configured ? "configured · submit to rotate" : "ghp_…"}
                  onChange={setGithubToken}
                />

                <div className="flex flex-wrap items-center gap-3">
                  <Switch
                    id="settings-auto-draft"
                    checked={autoDraft}
                    disabled={readOnly}
                    onCheckedChange={setAutoDraft}
                    aria-label="Open a draft pull request automatically"
                  />
                  <label htmlFor="settings-auto-draft" className="min-w-0 flex-1 cursor-pointer">
                    <span className="block text-sm font-medium text-foreground">Auto-draft pull requests</span>
                    <span className="block text-xs text-muted-foreground">
                      A validated Proposal may open a draft PR at Proposed; merging always stays human.
                    </span>
                  </label>
                </div>

                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    size="sm"
                    disabled={readOnly || busy === "repo"}
                    onClick={() => {
                      void save("repo", {
                        repository: {
                          owner: owner.trim(),
                          repo: repo.trim(),
                          branch: branch.trim(),
                          autoDraftPR: autoDraft,
                          ...(githubToken === "" ? {} : { token: githubToken }),
                        },
                      });
                    }}
                  >
                    Save repository
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={busy === "repo"}
                    onClick={() => {
                      void test("repo", "github");
                    }}
                  >
                    Test repository access
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={readOnly || settings.repository.token.configured === false}
                    onClick={() => {
                      void save("repo", { repository: { token: "" } });
                    }}
                  >
                    Remove token
                  </Button>
                  {feedback.repo !== undefined && feedback.repo.text !== "" && (
                    <span
                      role="status"
                      className={feedback.repo.tone === "error" ? "text-sm text-destructive" : "text-sm text-muted-foreground"}
                    >
                      {feedback.repo.text}
                    </span>
                  )}
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <Terminal className="size-4 text-primary" aria-hidden="true" /> Connectors
                  <Badge variant="mint">
                    <Lock className="size-3" aria-hidden="true" /> native_k8s_logs always on
                  </Badge>
                </CardTitle>
                <CardDescription>
                  Native Kubernetes logs need no dependency and cannot be switched off. Every other connector
                  is opt-in, query-only, and degrades the evidence bundle instead of failing the Page.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border-subtle">
                {connectors.map((connector) => (
                  <ConnectorRow
                    key={`${connector.name}:${connector.url}:${String(connector.enabled)}:${connector.token.fingerprint ?? ""}:${String(readOnly)}`}
                    connector={connector}
                    readOnly={readOnly}
                    busy={busy === `connector:${connector.name}`}
                    onSave={(name, patch) => {
                      void save(`connector:${name}`, { connectors: [{ name, ...patch }] });
                    }}
                    onTest={(target) => {
                      void test(target, target);
                    }}
                    testResult={feedback[`connector:${connector.name}`]}
                  />
                ))}

                <div className="mt-1 flex flex-col gap-1">
                  <span className="text-sm font-medium text-foreground">Alertmanager webhook</span>
                  <div className="flex flex-wrap items-center gap-2">
                    <code className="min-w-0 flex-1 overflow-x-auto rounded-md border border-border bg-panel px-2 py-1.5 font-mono text-xs whitespace-nowrap text-foreground">
                      {settings.alertmanagerWebhookUrl}
                    </code>
                    <Button
                      size="sm"
                      variant="secondary"
                      aria-label="Copy the Alertmanager webhook URL"
                      onClick={() => void copy(settings.alertmanagerWebhookUrl)}
                    >
                      {copied ? <Check className="size-3.5" aria-hidden="true" /> : <Copy className="size-3.5" aria-hidden="true" />}
                      {copied ? "Copied" : "Copy"}
                    </Button>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    Point Alertmanager&apos;s webhook_configs at this URL. Delivery is authenticated per sender.
                  </p>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <Server className="size-4 text-primary" aria-hidden="true" /> Version and external ownership
                </CardTitle>
                <CardDescription>
                  The same version <span className="font-mono text-foreground">kubectl page version</span> prints,
                  and the commands an external secret manager owns.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-2">
                <p className="font-mono text-sm text-foreground">pagerless-server {settings.version}</p>
                <p className="text-sm text-muted-foreground">
                  Credentials: <span className="font-mono text-foreground">{settings.managedBy}</span>
                  {settings.writeCredentials ? " · the Portal may write them" : " · the Portal is read-only"}
                </p>
                <CommandBlock
                  flat
                  command={`kubectl page config get && kubectl page version -o json`}
                  note="Masked fingerprints and the server version, without a browser."
                />
                <CommandBlock
                  flat
                  command={`kubectl -n pagerless get secret pagerless-credentials -o jsonpath='{.data.AI_API_KEY}' | base64 -d`}
                  note="Only an operator with get on the named Secret can read it; the Portal never can."
                />
              </CardContent>
            </Card>
          </>
        )}
      </main>

      <CliDrawer open={cliOpen} onOpenChange={setCliOpen} page={null} />
    </div>
  );
}
