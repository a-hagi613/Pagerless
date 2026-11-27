# Release engineering

One release is one immutable tag: `pagerless-v<major>.<minor>.<patch>`. The
tag, the GitHub Release, the two image tags, and the chart version are written
once. A later fix ships as a new version.

The earlier v1.0.0 and v2.0.0 tags and their distribution artifacts belong to
the retired operator. They are frozen: no workflow, script, or manual command
in this repository targets them, and the release workflow does not listen to
v-prefixed tags.

## Coordinates

| Artifact | Coordinate |
| --- | --- |
| GitHub Release | `pagerless-v<version>` |
| Server image | `ghcr.io/a-hagi613/pagerless/core/server:<version>` |
| Agent image | `ghcr.io/a-hagi613/pagerless/core/agent:<version>` |
| Chart | `oci://ghcr.io/a-hagi613/pagerless/core/charts/pagerless:<version>` |
| Chart fallback | the `pagerless-<version>.tgz` asset on the GitHub Release |

`hack/versions.env` is the single source of truth for the image, chart, tag
prefix, and tool versions. The chart defaults, the kind integration, and the
release workflow all read it. `scripts/release/check-versions.sh` fails when
the chart version, the image tags, the image repositories, the Krew manifest,
or the generated Homebrew formula disagree.

## What the workflow does

`.github/workflows/release.yml` runs only for `pagerless-v*.*.*` or an explicit
`workflow_dispatch` of such a tag.

1. **validate** checks the tag is prefixed and semver-shaped, points at HEAD,
   has no published GitHub Release (an unfinished draft may resume), matches
   the chart version, and leaves no retired image or chart coordinate anywhere
   in the tree. It also runs the release guard tests.
2. **verify** builds the four CLI archives, `checksums.txt`, four SBOMs, the
   rendered Krew manifest, both images, and the chart. It installs the
   linux/amd64 archive and the Krew manifest through a local HTTP server, then
   uploads the exact `dist/` directory.
3. **publish** waits for approval on the `release` environment. It creates a
   **draft** GitHub Release first, then pushes the multi-arch images and the
   chart, uploading each manifest digest (and the chart tarball SHA-256) to
   the draft as it goes. A re-run compares those recorded digests and refuses
   to overwrite a different artifact. Only after every asset is uploaded does
   it publish the draft and promote it as GitHub's latest release, so users
   discovering Latest land on the real 1.0 instead of the retired v2.0.0. No
   legacy tag or release is modified. A partial first publication therefore
   resumes instead of starting over.
4. **access** is a separate, credential-free job that runs after publication.
   It downloads and installs the public release, checks every coordinate
   anonymously, and pulls both images and the chart with empty credential
   stores. A private image or chart fails this job clearly. After the owner
   makes the three GHCR packages public, re-run only this job; no publication
   runs again.

GoReleaser OSS rejects a prefixed tag as non-semver. The workflow therefore
validates the tag itself and passes the version through `SNAPSHOT_VERSION`;
GoReleaser builds and verifies the artifacts, and the workflow owns the tag and
the GitHub Release. `.goreleaser.yaml` has publication disabled so a local
`goreleaser release` can never publish.

## Owner actions

The GitHub token needs `contents: write` and `packages: write`; the workflow
declares exactly those permissions on the publish job. A GitHub environment
named `release` with required reviewers gates publication.

GHCR packages are private until their visibility is changed. The anonymous
access gate fails until the server, agent, and chart packages are public in the
repository package settings; re-run only the `Anonymous install gate` job
afterwards. Until then the GitHub Release chart tarball is the documented
fallback and is anonymously downloadable.

GitHub immutable releases are disabled for this repository, so demo media can
be attached after publication without rewriting the release:

```sh
gh release upload pagerless-v1.0.0 /path/to/demo.mp4 --repo a-hagi613/pagerless
```

The workflow does not embed binaries in Git and does not lock the asset list.

## Publishing once

Only the coordinator publishes, after the release commit is on `main` and every
gate is green:

```sh
git tag -a pagerless-v1.0.0 -m "Pagerless 1.0.0" <immutable-sha>
git push origin pagerless-v1.0.0
```

The tag must be created once and never moved. A partial first publication
resumes from its draft and recorded digests. Once the release is public it is
never rewritten: a defect ships as the next patch version.

## Verification before tagging

```sh
bash scripts/release/check-versions.sh --version 1.0.0
bash scripts/release/check-release-target.sh --tag pagerless-v1.0.0 --offline
bash scripts/release/test-guards.sh
make release-check
make release-snapshot RELEASE_SNAPSHOT_VERSION=1.0.0
helm lint charts/pagerless
make validate
```

After publication:

```sh
bash scripts/release/verify-artifacts.sh --version 1.0.0
bash scripts/release/verify-public-access.sh --tag pagerless-v1.0.0 --version 1.0.0
```
