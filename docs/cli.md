# The `kubectl page` CLI

`kubectl-page` is a kubectl plugin: kubectl resolves `kubectl page` to it. It
never calls GitHub. A command runs in one of two modes, reports the mode it used
on standard error, and never falls back to the other mode:

- **kubeconfig mode** (the default) reads and patches `Page` resources in the
  release namespace through the Kubernetes API, with the identity and RBAC of
  your kubeconfig.
- **server mode** (`--mode server`) calls the Portal's HTTP API with the session
  that `kubectl page login` stored.

```
kubectl page new <kind>/<name>   open a Page for a Target
kubectl page get <page>          show one Page (synonym: show)
kubectl page list                list Pages, newest first
kubectl page diff <page>         show the Proposal against its recorded base
kubectl page approve <page>      approve the displayed Proposal hash
kubectl page reject <page>       reject it (synonym: dismiss)
kubectl page config get          print the masked settings
kubectl page config set …        write one section of the credentials Secret
kubectl page config unset <KEY>  clear one credential key
kubectl page login               store a session for server mode
kubectl page version             print build, expected chart, and mode
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | The command succeeded. |
| `1` | The command failed: the cluster, the server, or the write. |
| `2` | The command line was invalid. |
| `3` | A write was refused because a secret manager owns the credentials Secret. |
| `4` | The Proposal changed between display and decision. |

## Global flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig` | `$KUBECONFIG`, then `~/.kube/config` | Kubeconfig file. |
| `--context` | the current context | Kubeconfig context. |
| `-n`, `--namespace` | the current context's namespace | Namespace of the Target. |
| `--release-namespace` | `pagerless` | Namespace that holds Pages and the credentials Secret. |
| `--mode` | `kubeconfig`, or `server` when `--server` is given | Transport. |
| `--server` | empty | Portal base URL; implies `--mode server`. |
| `--config` | `${XDG_CONFIG_HOME:-~/.config}/pagerless/config.yaml` | Local configuration file. |
| `--credentials-secret` | `pagerless-credentials` | Secret written by `config` in kubeconfig mode. |

The local configuration holds only a server URL and an optional session token,
is written with mode `0600`, and never contains an AI or repository credential.

## `kubectl page new`

Opens a Page for the Target the argument names, or prints the Active Page that
already covers the same Fingerprint. A bare name is refused, because a
Deployment and a Job may share it; the error lists the accepted forms.

```
kubectl page new pod/checkout-7d9f8b6c5-x2x4q
kubectl page new deployment/checkout -n shop --hint "after the 14:10 deploy"
kubectl page new node/ip-10-0-1-17
```

| Target form | Meaning |
| --- | --- |
| `deployment/<name>` (or `deploy/`) | The Deployment. |
| `statefulset/<name>` (or `sts/`) | The StatefulSet. |
| `daemonset/<name>` (or `ds/`) | The DaemonSet. |
| `job/<name>` | The Job. |
| `cronjob/<name>` (or `cj/`) | The CronJob. |
| `pod/<name>` | A Pod; in kubeconfig mode a pod of a Deployment or StatefulSet is resolved to its owner, and the pod is recorded as the Trigger's pod. |
| `pvc/<name>` (or `persistentvolumeclaim/`) | The PersistentVolumeClaim. |
| `node/<name>` | The Node. It is cluster-scoped, so it takes no namespace. |

`--hint` passes guidance to the agent (at most 1000 characters). Output is one
line: `page.pagerless.dev/<name> created`, or `… already active`.

## `kubectl page list`

Lists Pages newest first with the resource's own printer columns: `NAME`,
`TARGET`, `PHASE`, `TRIGGER`, `PR`, `AGE`. `-o json` and `-o yaml` print the
transport's list object.

## `kubectl page get`

Prints the Diagnosis, Findings, Proposal with its rendered change, decision,
pull request, Verification checks, and Narrative. `-o json` and `-o yaml` print
the Page resource (kubeconfig mode) or the Portal's record (server mode).

`--watch` (kubeconfig mode only) prints the agent's steps and Phase changes as
they happen, then the presentation once the Page is Proposed or ends.

## `kubectl page diff`

Prints the Proposal's rendered change as a unified diff with the base commit and
Target generation it applies to. `-o json` and `-o yaml` print the Proposal.

## `kubectl page approve`

Shows the rendered change, then records an approval bound to the displayed
Proposal hash. If the Proposal changes before the decision is recorded, the
command exits `4` and nothing is approved. `--yes` skips the confirmation and
`--reason` records why. A Proposal that changes no file acknowledges the
Diagnosis. In kubeconfig mode `--by` records an identity; server mode records
the signed-in subject and refuses `--by`.

## `kubectl page reject`

Rejects the Proposal, bound to the same hash, and requires `--reason`. A
rejection is final: the Page ends as Rejected, and a new attempt is a new Page.

## `kubectl page config`

| Command | Meaning |
| --- | --- |
| `config get [-o json\|yaml]` | The masked settings: `{configured, fingerprint, updatedAt}` per key, the owner, and whether onboarding is still required. |
| `config set ai` | `--ai-base-url`, `--ai-model`, and the API key from a prompt, stdin, a file, or an env file. |
| `config set github` | `--github-repo`, `--github-api-url`, and the token from the same sources. |
| `config set connector --name <name>` | The connector's `CONNECTOR_<NAME>_TOKEN`. |
| `config set server` | Stores a session; the same as `login`. |
| `config unset <KEY>` | Clears one key without touching the others. |

A write merges only the submitted keys. When `credentials.existingSecret` names
an externally managed Secret, every write is refused with `409`, the manager's
name, and exit code `3`, and no write is attempted.

### Secrets are never flags

`--api-key`, `--token`, `--password`, and equivalents are rejected with a
message listing the accepted forms:

- an interactive prompt that reads without echo;
- `--from-stdin`, as `KEY=value` lines;
- `--from-file <path>`, as `KEY=value` lines;
- `--from-env-file <path>`, the same format.

A value therefore never reaches shell history, a process listing, or a rendered
manifest.

## `kubectl page login`

Exchanges the bootstrap token from `pagerless-admin-secret` for a session and
stores the server URL and the session in the local configuration with mode
`0600`. The token is read from the same four sources as a credential and is
never stored. The command prints the step that deletes the bootstrap Secret.

## `kubectl page version`

Prints the plugin version, commit, build date, the chart version it expects, the
Go version, and the mode. In server mode it also prints the server's version and
`auth.mode`.
