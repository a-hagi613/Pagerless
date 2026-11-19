# The `kubectl page` CLI

`kubectl-page` is a kubectl plugin: kubectl resolves `kubectl page` to it. Every
command reads and patches `Page` resources (`pages.pagerless.dev/v1alpha1`) in
the `pagerless` namespace through the Kubernetes API, using the identity in your
kubeconfig. The plugin never calls `pagerless-server`.

```
kubectl page <target> [flags]      start a Page for a failing Target
kubectl page list                  list Pages, most recent first
kubectl page show <page>           show one Page
kubectl page approve <page>        approve the displayed Proposal
kubectl page dismiss <page>        reject the Proposal
kubectl page version               print build and expected chart version
```

## Global flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig` | `$KUBECONFIG`, then `~/.kube/config` | Path to the kubeconfig file. |
| `--context` | the current context | Kubeconfig context to use. |
| `-n`, `--namespace` | the current context's namespace | Namespace of the Target. Pages always live in `pagerless`. |

## `kubectl page <target>`

Starts a Page for the Target the argument names, or prints the Active Page that
already covers the same Fingerprint.

```
kubectl page pod/checkout-7d9f8b6c5-x2x4q
kubectl page deploy/checkout --namespace shop
kubectl page sts/ledger --namespace payments --hint "after the 14:10 deploy"
```

| Target form | Meaning |
| --- | --- |
| `pod/<name>` | The pod is resolved to the Deployment or StatefulSet that owns it; the pod is recorded as the Trigger's pod. |
| `deploy/<name>` (or `deployment/<name>`) | The Deployment itself is the Target. |
| `sts/<name>` (or `statefulset/<name>`) | The StatefulSet itself is the Target. |

`Page` accepts more Target kinds than the CLI can start from a name: a
`Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob`, `Pod`,
`PersistentVolumeClaim`, or `Node`. The watcher opens Pages for the kinds it can
observe; a human request through this command is limited to the three forms above.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--hint` | empty | Guidance for the agent. At most 1000 characters. |

Output is one line: `page.pagerless.dev/<name> created`, or
`page.pagerless.dev/<name> already active` when an Active Page already covers
the Fingerprint.

## `kubectl page list`

Lists Pages in `pagerless`, newest first.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-o`, `--output` | table | `json` or `yaml`. |

An empty cluster prints `No Pages found in namespace "pagerless".` in table form
and an empty `PageList` in JSON or YAML.

## `kubectl page show <page>`

Shows the Page's Target, Phase, Diagnosis, Findings, Proposal and rendered change,
pull request, Verification checks, and Narrative.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-o`, `--output` | table | `json` or `yaml`. |
| `--watch` | `false` | Follow the agent's steps until the Page stops changing, then print the presentation. Cannot be combined with `--output`. |

## `kubectl page approve <page>`

Shows the Proposal and, after confirmation, approves exactly its hash. If the
Proposal changed before the approval was admitted, the approval is refused and
nothing is approved.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-y`, `--yes` | `false` | Approve without confirmation. |
| `--reason` | empty | Reason recorded with the approval. At most 1000 characters. |
| `--by` | the kubeconfig user | Identity recorded as the approver. The default comes from a `SelfSubjectReview`, then `$USER`. |

Approving a Proposal that makes no change acknowledges the Diagnosis. Any answer
other than `y` or `yes` is a refusal, including an input that ends without an
answer.

## `kubectl page dismiss <page> --reason <reason>`

Rejects the Proposal. A Rejection is final for its Page; a new attempt is a new
Page, optionally with a hint.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--reason` | required | Why the Proposal is rejected. At most 1000 characters. |
| `--by` | the kubeconfig user | Identity recorded as the approver. The default comes from a `SelfSubjectReview`, then `$USER`. |

## `kubectl page version`

Prints the version, commit, build date, the chart version the binary expects, and
the Go toolchain.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-o`, `--output` | table | `json`. |

```console
$ kubectl page version
kubectl-page 2.0.0
commit 4f3c2a1b9e8d7c6b5a4938271605f4e3d2c1b0a9
built 2026-11-19T03:46:39Z
expected chart 2.0.0
go go1.27.4
```

`version`, `commit`, `built`, and `expected chart` are injected at link time by
the release, so a development build reports `dev`, `none`, `unknown`, and the
chart version in the tree. A release always reports the chart version it ships
with, so `kubectl page version` and `helm list` can be compared directly.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | The command completed. |
| `1` | The command failed: invalid arguments or flags, a refused confirmation, a Page that does not exist, or a Kubernetes API error. |

There are no other exit codes. A script that approves a Proposal should treat
`1` as "not approved" and read the message on standard error for the reason.

## Permissions

| Action | Required permission |
| --- | --- |
| `list`, `show` | `get`, `list`, and `watch` on `pages` in `pagerless`. |
| `kubectl page <target>` | `create` on `pages` in `pagerless`. |
| `approve`, `dismiss` | `patch` on `pages` in `pagerless`. |

`Role pagerless-approver` grants `get`, `list`, `watch`, and `patch` on `pages`.
The chart binds it when `approvers.subjects` is set. A decision is an annotation
patch on the Page, not a status write, so an approver never needs
`pages/status`.
