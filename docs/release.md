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

## Package visibility

The workflow publishes to GHCR with the repository's `GITHUB_TOKEN`, so the
`pagerless-server`, `pagerless-agent`, and `charts/pagerless` packages inherit
the repository's visibility. For the install commands in the README to work
without a credential, each package must be public. The verification job logs in
to GHCR, so it proves the artifacts, not anonymous access; check the three
packages under the repository's package settings after the first release.

## Before the tag

```bash
make release-preflight   # version wiring, goreleaser check, every artifact, verified
```

`make release-check` and `make release-snapshot` remain available on their own,
and CI runs them on every pull request. `release-preflight` chains the version
check to both, so one command proves that the tree, the chart, and the plugin
manifest agree and that the archives, checksums, SBOMs, and rendered manifest all
build and verify.

The snapshot reports `0.0.0-snapshot` and publishes nothing. **The publish stage
itself cannot be exercised without a tag**, because it needs one and a token: a
GoReleaser publish-time validation, such as its rule that a Krew publisher field
may hold only a single environment reference, fails there and nowhere earlier.
Review `.goreleaser.yaml` and `.github/workflows/release.yml` for that class of
problem, and rely on `release.mode: replace` so a release that fails after the
release object exists can be re-run for the same tag instead of needing a manual
delete.

Bump the version in `charts/pagerless/Chart.yaml` (`version` and `appVersion`) in
the release pull request. `scripts/release/check-versions.sh --version X.Y.Z`
checks that the chart version, the appVersion, and the image tags agree with the
version before you tag it. `plugins/page.yaml` is the manifest template; no
committed copy can carry checksums before the archives exist. The release job
renders it from `dist/checksums.txt` after the tag, publishes the rendered file
as the `page.yaml` asset, and opens a refresh Pull Request when the committed
copy differs. The lint fails on a `${...}` placeholder, so an unrendered
template cannot reach the index or a release.

## After the tag

The workflow publishes in this order: validate, GoReleaser, images, chart, verify,
formula. It is not complete until both verification jobs pass:

- `verify` creates a throwaway kind cluster, installs the chart from GHCR with an
  `existingSecret`, checks that exactly one Deployment and no StatefulSet was
  rendered, runs the read-only authorization matrix against that install, installs
  the plugin from the published `page.yaml`, and checks that both images carry
  both architectures. It deletes the cluster even on failure.
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
