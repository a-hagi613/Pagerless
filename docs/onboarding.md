# Onboarding

An installation is complete when Pagerless can reach an AI endpoint and, if it is
to open pull requests, a Git repository. Both are read at runtime from
`pagerless-credentials` in the release namespace. This page covers the three
paths to that state, and what an installation that stops short of it can still
do.

## What a completed installation holds

| Key | Used for |
| --- | --- |
| `AI_BASE_URL` | The OpenAI-compatible endpoint of the model provider. |
| `AI_API_KEY` | The key for that endpoint. |
| `AI_MODEL` | The model name the agent requests. |
| `GITHUB_TOKEN` | The credential that opens the Pull Request for an approved Proposal. |
| `GITHUB_REPO` | The Target Repository, as `owner/name`. |
| `GITHUB_API_URL` | The GitHub API base URL. Empty uses `https://api.github.com`. |

Connector tokens are separate: a connector reads `CONNECTOR_<UPPER_NAME>_TOKEN`
from the same Secret. None of the six keys above is required for the server to
start.

## Path 1: the Portal

Availability: delivered by the Portal settings route in the remaining v2.0
slices. The route is `/settings`, and it writes the same six keys into the same
Secret.

1. Read the bootstrap command from the release notes:

   ```bash
   helm get notes pagerless --namespace pagerless
   ```

2. Run the printed `kubectl port-forward` command and open
   `http://localhost:8080`.
3. Sign in with the bootstrap token the notes print.
4. Fill the AI and GitHub fields on `/settings` and save.

The Portal shows the fields read-only when `credentials.existingSecret` names a
Secret the chart does not own, because writing it would break the guarantee that
your secret manager is the only writer.

## Path 2: the CLI

Availability: delivered by `kubectl page config set` in the remaining v2.0
slices.

```bash
kubectl page config set ai          # prompts without echo, or reads stdin
kubectl page config set github
kubectl page config show            # keys present and set, never their values
```

The command writes `pagerless-credentials` through the Kubernetes API and never
takes a key on the command line, so a key does not appear in a shell history or a
process listing.

## Path 3: your secret manager

This path is available with the chart as it ships. The chart stops rendering the
Secret when you name an existing one:

```bash
kubectl -n pagerless create secret generic team-credentials \
  --from-literal=AI_BASE_URL=https://models.example.com/v1 \
  --from-literal=AI_API_KEY=... \
  --from-literal=AI_MODEL=... \
  --from-literal=GITHUB_TOKEN=... \
  --from-literal=GITHUB_REPO=owner/repo \
  --from-literal=GITHUB_API_URL=https://api.github.com

helm upgrade --install pagerless ./charts/pagerless -n pagerless \
  --set credentials.existingSecret=team-credentials
```

The Secret must exist before the release is installed. A secret manager that
owns the Secret keeps owning it across chart upgrades, because the chart renders
no Secret when `credentials.existingSecret` is set.

## Finishing an installation without credentials

An installation without credentials is a supported state, not a broken one:

- The chart creates `pagerless-credentials` with every key present and empty, so
  the pod mounts a complete volume instead of failing on a missing file.
- `pagerless-server` starts, serves the agent report API and the metrics API,
  and answers `/healthz` and `/readyz`.
- Nothing needs to be reinstalled. Fill the Secret, or point
  `credentials.existingSecret` at one, and restart the Deployment so the pod
  re-reads it:

  ```bash
  kubectl -n pagerless rollout restart deployment/pagerless-server
  ```

Check what an installation holds without printing values:

```bash
kubectl -n pagerless get secret pagerless-credentials \
  -o jsonpath='{range $k, $v := .data}{$k}: {if $v}set{else}empty{end}{"\n"}{end}'
```

An installation that holds AI credentials but no GitHub credentials can still
diagnose a Page: it ends the Page as Diagnosed with no change recommended, because
Pagerless only changes a cluster through a pull request.

## Next

- [`docs/install.md`](install.md) for the chart, the watch modes, and the values.
- [`docs/auth.md`](auth.md) for how the Portal authenticates an operator.
- [`docs/cli.md`](cli.md) for the terminal equivalents of every action.
