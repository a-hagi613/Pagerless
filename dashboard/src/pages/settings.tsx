import { Check, Copy, KeyRound, Lock, Plus, Server, ShieldCheck, Terminal } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

import { AppHeader } from "@/components/app-header";
import { CliDrawer } from "@/components/cli-drawer";
import { CommandBlock } from "@/components/command-block";
import { Notice } from "@/components/notices";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ApiError,
  getSettings,
  getVersion,
  testSettings,
  UnauthenticatedError,
  unsetSetting,
  updateSettings,
  type SettingsPatch,
  type TestTarget,
} from "@/lib/api";
import { signOut } from "@/lib/sign-out";
import { shortHash } from "@/lib/format";
import type { MaskedField, Settings, VersionInfo } from "@/lib/types";
import { useCopy } from "@/lib/use-copy";
import { useSession } from "@/lib/use-session";

type Tone = "info" | "success" | "error";
type Feedback = { tone: Tone; text: string };

const connectorLabels: Record<string, string> = {
  native_k8s_logs: "Kubernetes API logs",
  loki: "Grafana Loki",
  prometheus: "Prometheus",
  cloudwatch: "CloudWatch",
  datadog: "Datadog",
  alertmanager: "Alertmanager",
};

function connectorLabel(name: string): string {
  return connectorLabels[name] ?? name;
}

function MaskedChip({ value }: { value: MaskedField }) {
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
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  masked: MaskedField;
  readOnly: boolean;
  placeholder: string;
  onChange: (next: string) => void;
}) {
  return (
    <div className="flex flex-wrap items-end gap-2">
      <label className="flex min-w-0 flex-1 flex-col gap-1 text-sm font-medium text-foreground" htmlFor={id}>
        {label}
        <Input
          id={id}
          data-testid={id}
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

export function SettingsPage() {
  const { session, refresh: refreshSession } = useSession();
  const [settings, setSettings] = useState<Settings | null>(null);
  const [version, setVersion] = useState<VersionInfo | null>(null);
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
  const [apiUrl, setApiUrl] = useState("");
  const [githubToken, setGithubToken] = useState("");

  // Connector drafts, keyed by connector name.
  const [connectorTokens, setConnectorTokens] = useState<Record<string, string>>({});
  const [newConnector, setNewConnector] = useState("");
  const [newConnectorToken, setNewConnectorToken] = useState("");

  const readOnly = settings !== null && (settings.managedBy === "external" || !settings.writeCredentials);

  const applySettings = useCallback((value: Settings) => {
    setSettings(value);
    setBaseUrl(value.ai.baseUrl);
    setModel(value.ai.model);
    setOwner(value.repository.owner);
    setRepo(value.repository.repo);
    setApiUrl(value.repository.apiUrl ?? "");
  }, []);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const [next, info] = await Promise.all([getSettings(signal), getVersion(signal)]);
        applySettings(next);
        setVersion(info);
        setLoadError(null);
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        if (cause instanceof UnauthenticatedError) {
          void refreshSession();
          return;
        }
        setLoadError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setLoading(false);
      }
    },
    [applySettings, refreshSession],
  );

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => {
      controller.abort();
    };
  }, [load]);

  const say = (key: string, tone: Tone, text: string) => {
    setFeedback((current) => ({ ...current, [key]: { tone, text } }));
  };

  const save = async (key: string, patch: SettingsPatch) => {
    setBusy(key);
    setFeedback((current) => ({ ...current, [key]: { tone: "info", text: "" } }));
    try {
      const next = await updateSettings(patch);
      applySettings(next);
      setApiKey("");
      setGithubToken("");
      setConnectorTokens({});
      say(key, "success", "Saved. The masked fingerprint is the proof the write landed.");
    } catch (cause) {
      if (cause instanceof UnauthenticatedError) {
        void refreshSession();
        return;
      }
      if (cause instanceof ApiError && cause.code === "settings_externally_managed") {
        // The API names the manager in the error; re-reading the view is the authoritative way to render
        // the read-only state, and the refusal stays inline rather than in a dialog.
        await load();
        say(
          key,
          "info",
          "This credential Secret is managed externally, so the write was refused. Every field is read-only.",
        );
        return;
      }
      say(key, "error", cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(null);
    }
  };

  const remove = async (key: string, credential: string) => {
    setBusy(key);
    try {
      const next = await unsetSetting(credential);
      applySettings(next);
      say(key, "success", `Cleared ${credential}.`);
    } catch (cause) {
      if (cause instanceof UnauthenticatedError) {
        void refreshSession();
        return;
      }
      if (cause instanceof ApiError && cause.code === "settings_externally_managed") {
        await load();
        say(key, "info", "This credential Secret is managed externally, so the write was refused.");
        return;
      }
      say(key, "error", cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(null);
    }
  };

  const test = async (key: string, target: TestTarget) => {
    setBusy(key);
    setFeedback((current) => ({ ...current, [key]: { tone: "info", text: "Testing…" } }));
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
  const missingAI =
    settings !== null &&
    (!settings.ai.apiKey.configured || settings.fields.AI_BASE_URL?.configured === false);
  const missingRepository =
    settings !== null &&
    (!settings.repository.token.configured || settings.fields.GITHUB_REPO?.configured === false);

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
        version={version?.version ?? "dev"}
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
            The AI endpoint, the Target Repository, and the Connectors every Page uses. Each control has a{" "}
            <span className="font-mono text-foreground">kubectl page config</span> equivalent.
          </p>
        </div>

        {loadError !== null && <Notice tone="error">Settings could not be read: {loadError}</Notice>}

        {settings?.onboarding === true && (
          <Notice tone="info">
            <span className="font-semibold">This installation is unfinished.</span> Pagerless serves the
            Portal and stays in the Service, but it cannot investigate a Page until{" "}
            {missingAI && missingRepository
              ? "the AI endpoint and the Target Repository are configured"
              : missingAI
                ? "the AI endpoint is configured"
                : "the Target Repository is configured"}
            . Fill in the cards below; nothing else is required.
          </Notice>
        )}

        {readOnly && (
          <div data-testid="settings-external-banner">
            <Notice tone="info">
              <span className="font-semibold">
                Managed externally by Secret <span className="font-mono">{settings?.manager}</span>.
              </span>{" "}
              Every field below is configured and read-only; a write is refused with{" "}
              <span className="font-mono">409 settings_externally_managed</span>. Read the masked state with{" "}
              <span className="font-mono">kubectl page config get</span>, or copy the command below.
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
                    {settings.ai.apiKey.configured ? "key configured" : "needs a key"}
                  </Badge>
                </CardTitle>
                <CardDescription>
                  Any OpenAI-compatible endpoint: OpenAI, DeepSeek, Ollama, vLLM, or a gateway. The key is
                  written to the credentials Secret and never returns to the browser.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
                    disabled={busy === "ai" || !settings.ai.apiKey.configured}
                    onClick={() => {
                      void test("ai", "ai");
                    }}
                  >
                    Test connection
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={readOnly || !settings.ai.apiKey.configured}
                    onClick={() => {
                      void remove("ai", "AI_API_KEY");
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
                  <label className="flex flex-col gap-1 text-sm font-medium text-foreground" htmlFor="settings-repo-api-url">
                    API URL (optional)
                    <Input
                      id="settings-repo-api-url"
                      value={apiUrl}
                      readOnly={readOnly}
                      onChange={(event) => {
                        setApiUrl(event.target.value);
                      }}
                      className="font-mono text-sm"
                      placeholder="https://api.github.com"
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

                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    size="sm"
                    data-testid="settings-repo-save"
                    disabled={readOnly || busy === "repo"}
                    onClick={() => {
                      const repository: NonNullable<SettingsPatch["repository"]> = {};
                      if (owner.trim() !== "" && repo.trim() !== "") {
                        repository.owner = owner.trim();
                        repository.repo = repo.trim();
                      }
                      if (apiUrl.trim() !== "") {
                        repository.apiUrl = apiUrl.trim();
                      }
                      if (githubToken !== "") {
                        repository.token = githubToken;
                      }
                      void save("repo", { repository });
                    }}
                  >
                    Save repository
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    data-testid="settings-repo-test"
                    disabled={busy === "repo" || !settings.repository.token.configured}
                    onClick={() => {
                      void test("repo", "github");
                    }}
                  >
                    Test repository access
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={readOnly || !settings.repository.token.configured}
                    onClick={() => {
                      void remove("repo", "GITHUB_TOKEN");
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
                    <Lock className="size-3" aria-hidden="true" /> native_k8s_logs needs no token
                  </Badge>
                </CardTitle>
                <CardDescription>
                  Native Kubernetes logs need no dependency. Every other Connector is opt-in and read-only;
                  the credentials Secret holds its token, and the Connector catalogue holds its endpoint.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border-subtle">
                {connectors.map((connector) => {
                  const key = `connector:${connector.name}`;
                  const token = connectorTokens[connector.name] ?? "";
                  return (
                    <div key={connector.name} className="flex flex-col gap-2 py-2.5">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="min-w-0 flex-1">
                          <span className="block text-sm font-medium text-foreground">{connectorLabel(connector.name)}</span>
                          <span className="block font-mono text-xs text-muted-foreground">
                            CONNECTOR_{connector.name.toUpperCase()}_TOKEN
                          </span>
                        </span>
                        <MaskedChip value={connector.token} />
                      </div>
                      <div className="flex flex-wrap items-end gap-2">
                        <label
                          className="flex min-w-0 flex-1 flex-col gap-1 text-xs font-medium text-muted-foreground"
                          htmlFor={`${key}-token`}
                        >
                          Token (write-only)
                          <Input
                            id={`${key}-token`}
                            value={token}
                            readOnly={readOnly}
                            type="password"
                            autoComplete="off"
                            onChange={(event) => {
                              setConnectorTokens((current) => ({
                                ...current,
                                [connector.name]: event.target.value,
                              }));
                            }}
                            className="font-mono text-xs"
                            placeholder={connector.token.configured ? "configured · submit to rotate" : "not configured"}
                          />
                        </label>
                        <Button
                          size="xs"
                          variant="secondary"
                          disabled={readOnly || busy === key || token === ""}
                          onClick={() => {
                            void save(key, { connectors: [{ name: connector.name, token }] });
                          }}
                        >
                          Save token
                        </Button>
                        <Button
                          size="xs"
                          variant="outline"
                          disabled={busy === key || !connector.token.configured}
                          onClick={() => {
                            void test(key, `connector:${connector.name}`);
                          }}
                        >
                          Test connection
                        </Button>
                        <Button
                          size="xs"
                          variant="ghost"
                          disabled={readOnly || !connector.token.configured}
                          onClick={() => {
                            void remove(key, `CONNECTOR_${connector.name.toUpperCase()}_TOKEN`);
                          }}
                        >
                          Remove
                        </Button>
                      </div>
                      {feedback[key] !== undefined && feedback[key].text !== "" && (
                        <span
                          role="status"
                          className={
                            feedback[key].tone === "error" ? "text-xs text-destructive" : "text-xs text-muted-foreground"
                          }
                        >
                          {feedback[key].text}
                        </span>
                      )}
                    </div>
                  );
                })}

                {connectors.length === 0 && (
                  <p className="py-2 text-sm text-muted-foreground">
                    No Connector token is configured. Add one below, or leave the platform read-only.
                  </p>
                )}

                <div className="flex flex-col gap-2 py-2.5">
                  <span className="flex items-center gap-1.5 text-sm font-medium text-foreground">
                    <Plus className="size-3.5" aria-hidden="true" /> Add a Connector token
                  </span>
                  <div className="flex flex-wrap items-end gap-2">
                    <label
                      className="flex w-48 flex-col gap-1 text-xs font-medium text-muted-foreground"
                      htmlFor="settings-connector-name"
                    >
                      Connector
                      <Input
                        id="settings-connector-name"
                        value={newConnector}
                        readOnly={readOnly}
                        placeholder="loki"
                        onChange={(event) => {
                          setNewConnector(event.target.value);
                        }}
                        className="font-mono text-xs"
                      />
                    </label>
                    <label
                      className="flex min-w-0 flex-1 flex-col gap-1 text-xs font-medium text-muted-foreground"
                      htmlFor="settings-connector-token"
                    >
                      Token (write-only)
                      <Input
                        id="settings-connector-token"
                        value={newConnectorToken}
                        readOnly={readOnly}
                        type="password"
                        autoComplete="off"
                        onChange={(event) => {
                          setNewConnectorToken(event.target.value);
                        }}
                        className="font-mono text-xs"
                        placeholder="the Connector's read-only token"
                      />
                    </label>
                    <Button
                      size="xs"
                      variant="secondary"
                      disabled={
                        readOnly ||
                        busy === "connector:new" ||
                        newConnector.trim() === "" ||
                        newConnectorToken === ""
                      }
                      onClick={() => {
                        const name = newConnector.trim().toLowerCase();
                        void save("connector:new", { connectors: [{ name, token: newConnectorToken }] }).then(() => {
                          setNewConnector("");
                          setNewConnectorToken("");
                        });
                      }}
                    >
                      Add token
                    </Button>
                  </div>
                  {feedback["connector:new"] !== undefined && feedback["connector:new"].text !== "" && (
                    <span
                      role="status"
                      className={
                        feedback["connector:new"].tone === "error"
                          ? "text-xs text-destructive"
                          : "text-xs text-muted-foreground"
                      }
                    >
                      {feedback["connector:new"].text}
                    </span>
                  )}
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <Server className="size-4 text-primary" aria-hidden="true" /> Version and ownership
                </CardTitle>
                <CardDescription>
                  The same version <span className="font-mono text-foreground">kubectl page version</span> prints,
                  and the commands that read the same masked state without a browser.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-2">
                <p className="font-mono text-sm text-foreground">
                  pagerless-server {version?.version ?? "dev"} · auth {version?.mode ?? session.mode}
                </p>
                <p className="text-sm text-muted-foreground">
                  Credentials: <span className="font-mono text-foreground">{settings.managedBy}</span> by{" "}
                  <span className="font-mono text-foreground">{settings.manager}</span>
                  {settings.writeCredentials ? " · the Portal may write them" : " · the Portal is read-only"}
                </p>
                <CommandBlock
                  flat
                  command="kubectl page config get && kubectl page version -o json"
                  note="Masked fingerprints and the server version, without a browser."
                />
                {readOnly && (
                  <div className="flex flex-wrap items-center gap-2">
                    <Button
                      size="sm"
                      variant="secondary"
                      aria-label="Copy the command that reads the externally managed credentials"
                      onClick={() => void copy("kubectl page config get")}
                    >
                      {copied ? <Check className="size-3.5" aria-hidden="true" /> : <Copy className="size-3.5" aria-hidden="true" />}
                      {copied ? "Copied" : "Copy the read command"}
                    </Button>
                    <span className="text-xs text-muted-foreground">
                      The values live in the external Secret; Pagerless never writes or returns them.
                    </span>
                  </div>
                )}
              </CardContent>
            </Card>
          </>
        )}
      </main>

      <CliDrawer open={cliOpen} onOpenChange={setCliOpen} page={null} />
    </div>
  );
}
