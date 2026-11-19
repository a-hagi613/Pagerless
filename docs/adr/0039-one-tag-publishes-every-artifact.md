# ADR-0039: One tag publishes every artifact, and the published copy is verified

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

A stranger installs Pagerless from published artifacts and never checks out the
repository: `kubectl krew install page` for the plugin, `brew install
a-hagi613/tap/kubectl-page` for the same binary, `helm install pagerless
oci://ghcr.io/a-hagi613/charts/pagerless --version 2.0.0` for the operator, and
the images from GHCR. Four channels carry a version, and every channel is a place
where the version can drift or an artifact can be stale.

Two of those channels have a second hazard. The Krew index is a shared upstream
repository whose submissions are reviewed as Pull Requests; two automations that
update it produce two competing Pull Requests for one tag. The Homebrew formula
contains four SHA-256 values; a generator that writes them from anything other
than the release's own checksum file can publish a formula that no archive
matches.

Nothing in the product had ever proved that the published artifacts install. A
green test suite proves the tree, not the release.

## Decision

A release is one tag, one builder, one writer per channel, and one proof:

- **GoReleaser is the only builder of the plugin archives.** It builds
  `kubectl-page` for Linux and macOS on amd64 and arm64 with `CGO_ENABLED=0` and
  `-trimpath`, injects the version, commit, and commit date through linker flags,
  and emits four `tar.gz` archives, one SHA-256 checksum file, and one SBOM per
  archive.
- **The Krew index has exactly one publisher.** GoReleaser's Krew publisher
  commits to the `a-hagi613/krew-index` fork and opens the Pull Request against
  `kubernetes-sigs/krew-index`. When `KREW_INDEX_TOKEN` is absent it skips the
  upload; the release still publishes the archives, the checksums, and the
  rendered manifest, and the job summary states that the index step was skipped.
  A missing token never half-writes an index and never fails a release silently.
- **The Homebrew formula has exactly one writer.**
  `scripts/release/homebrew-formula.sh` regenerates `Formula/kubectl-page.rb` from
  the release checksum file and fails when a checksum line is missing. It installs
  the binary into `bin` and its test asserts the plugin's help output. GoReleaser's
  deprecated formula generator is not used, and no `sha256` is hand-edited.
- **One version names everything.** The tag, the chart `version` and `appVersion`,
  the image tags, the plugin version, and the formula's archives are the same
  version. `scripts/release/check-versions.sh` fails the release when they
  disagree.
- **The snapshot gate runs on every pull request.** `make release-snapshot` runs
  GoReleaser with `--snapshot`, so the configuration, the archives, the checksums,
  and the SBOMs are exercised with no tag, no registry credential, and no GitHub
  token.
- **The published copy is verified, on the platform it targets.** The final jobs
  download the release assets, install the chart from GHCR on a throwaway kind
  cluster, install the plugin from the published Krew manifest, run the plugin's
  version command, check both image manifests for both architectures, and audit
  the formula with `brew audit --strict` and install it with
  `brew install --build-from-source` on macOS. Any mismatch fails the release.
- **The repository plugin manifest is a release product.** The release job
  renders `plugins/page.yaml` from the published checksum file, publishes it as a
  release asset, and opens a Pull Request when the committed copy differs. The
  release asset is the authoritative manifest; the committed copy follows it.

## Consequences

- A release needs a tag and the two optional tokens. It cannot be built from a
  dirty tree, because GoReleaser refuses one.
- The repository copy of `plugins/page.yaml` can lag the newest release by
  minutes, until the refresh Pull Request merges. The release asset never lags,
  and the verification job installs the asset.
- The Krew publisher depends on a fork existing and on a token that can write to
  it. Without the fork the index step cannot succeed; the release says so instead
  of failing halfway.
- A snapshot is not a release: it reports `0.0.0-snapshot`, it has no tag, and
  nothing is published. It proves the configuration, not the release.
- Verifying the published copy costs a kind cluster, two multi-arch image pulls,
  and a macOS runner per release. That cost is the point: a release that cannot
  install is worse than a slow release.
- The formula's version is inferred from its URLs rather than written as a
  `version` stanza, because Homebrew's audit rejects the redundant stanza. The
  checksum file, not the formula, is the source of truth.
