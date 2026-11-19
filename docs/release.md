# Releasing

One tag publishes every artifact at one version, and the last jobs install the
published artifacts rather than trusting the build. This page is the operator's
view of `.github/workflows/release.yml`; the decision behind it is
[ADR-0039](adr/0039-one-tag-publishes-every-artifact.md).

## What a tag publishes

| Artifact | Where it lands | Written by |
| --- | --- | --- |
| Four `kubectl-page` archives, `checksums.txt`, four SBOMs | GitHub release `vX.Y.Z` | GoReleaser |
| The Krew index Pull Request | `a-hagi613/krew-index` fork, PR to `kubernetes-sigs/krew-index` | GoReleaser's Krew publisher, and nothing else |
| The rendered plugin manifest (`page.yaml`) | Release asset, and a refresh PR when `plugins/page.yaml` is stale | `scripts/release/render-krew-manifest.sh` |
| The formula (`kubectl-page.rb`) | Release asset, and a Pull Request to `a-hagi613/homebrew-tap` | `scripts/release/homebrew-formula.sh`, and nothing else |
| `pagerless-server` and `pagerless-agent` images, `linux/amd64` and `linux/arm64` | `ghcr.io/a-hagi613/pagerless-{server,agent}:X.Y.Z` | The release workflow |
| The chart | `oci://ghcr.io/a-hagi613/charts/pagerless:X.Y.Z` | The release workflow |

The tag must equal the chart's `version` and `appVersion`. The first job fails
before anything is published when it does not.

## Required secrets

| Secret | Used for | Without it |
| --- | --- | --- |
| `GITHUB_TOKEN` | The GitHub release, GHCR, and the refresh Pull Request. | The workflow cannot run. |
| `KREW_INDEX_TOKEN` | Writing to the `a-hagi613/krew-index` fork and opening the index Pull Request. | The index step is skipped and the job summary says so; the archives, checksums, and rendered manifest are still published. |
| `HOMEBREW_TAP_TOKEN` | Writing to `a-hagi613/homebrew-tap`. | The tap Pull Request is skipped and the job summary says so; the formula is still published as a release asset. |

`KREW_INDEX_TOKEN` needs write access to `a-hagi613/krew-index`, which is a fork
of `kubernetes-sigs/krew-index`. `HOMEBREW_TAP_TOKEN` needs write access to
`a-hagi613/homebrew-tap`.

## Before the tag

```bash
make release-check     # goreleaser check and the Krew manifest lint
make release-snapshot   # archives, checksums, SBOMs, and the rendered manifest
```

Both run on every pull request. The snapshot reports `0.0.0-snapshot` and
publishes nothing; it exists so that a broken configuration is found before a tag
rather than by a failed release.

Bump the version in `charts/pagerless/Chart.yaml` (`version` and `appVersion`) in
the release pull request. `scripts/release/check-versions.sh --version X.Y.Z`
checks that the chart version, the appVersion, and the image tags agree with the
version before you tag it. `plugins/page.yaml` is rendered after the tag, because
it can only describe archives that exist; the release job renders it from the
published checksums and opens a refresh Pull Request when the committed copy
differs.

## After the tag

The workflow publishes in this order: validate, GoReleaser, images, chart, verify,
formula. It is not complete until both verification jobs pass:

- `verify` creates a throwaway kind cluster, installs the chart from GHCR with an
  `existingSecret`, checks that exactly one Deployment and no StatefulSet was
  rendered, installs the plugin from the published `page.yaml`, and checks that
  both images carry both architectures. It deletes the cluster even on failure.
- `formula` generates the formula from the release checksums, runs
  `brew audit --strict`, installs with `brew install --build-from-source`, and
  checks the installed plugin's version.

A failed verification does not retract the release, because GitHub releases and
image manifests are immutable in place. Fix the cause, bump the patch version, and
tag again; do not move the tag.

## Verifying a published release locally

```bash
scripts/release/verify-artifacts.sh --version 2.0.0
```

The script downloads every archive and SBOM, verifies each against the release
checksum file, renders and lints the Krew manifest, installs it with
`kubectl krew install --manifest`, and runs `kubectl page version`. It needs
`kubectl`, `krew`, `curl`, and `tar`. `--skip-krew` checks the archives only, and
`--base-url` points the same path at a local HTTP server serving a snapshot.
