# ADR-0028: Pages replace Triages as the unit of investigation

- **Status:** Accepted
- **Date:** 2026-11-18
- **Supersedes:** nothing; amends [ADR-0001](0001-only-pagerless-server-writes-triage-status.md) and [ADR-0018](0018-deterministic-decisions-and-canonical-triages.md) for the new vocabulary.

## Context

`Triage` named an investigation that an alert, a CLI user, or the dashboard
started, and it could only target a Deployment or a StatefulSet. A failure now
arrives from a Kubernetes Warning Event, a Target status transition, an inbound
alert, or a human request, and it can be a CronJob that never completes, a
DaemonSet pod that cannot mount its config, a PVC that stays Pending, or a Node
under memory pressure. The v1 name describes none of those cases, and v1's
`TriageApproval` companion exists only because a decision had to be expressible
by a subject that could not patch the resource.

v2.0 is a new API rather than a rename, so the resource, its phases, its printer
columns, and the CLI change together in one visible break.

## Decision

The investigation resource is `Page`:

- `apiVersion: pagerless.dev/v1alpha1`, kind `Page`, plural `pages`, singular
  `page`, short name `pg`, category `pagerless`, namespaced, with a `status`
  subresource.
- `spec` is immutable, enforced by the CEL rule `self == oldSelf`. It records the
  Target reference (API version, kind, namespace, name, UID), the Fingerprint,
  the first Trigger, an optional Hint, an optional declared Target Repository,
  and the previous Page of a Recurrence.
- `TargetKind` is a closed enum of `Deployment`, `StatefulSet`, `DaemonSet`,
  `Job`, `CronJob`, `Pod`, `PersistentVolumeClaim`, and `Node`. A CEL rule on the
  target reference requires a non-empty namespace for every namespaced kind and
  rejects a namespace for `Node`.
- The Phase set is `Pending`, `Investigating`, `Proposed`, `Approved`,
  `PullRequestOpen`, `Verifying`, `Recovered`, `NotRecovered`, `Diagnosed`,
  `Rejected`, `Expired`, `Invalid`, `Superseded`, `Failed`. The last eight are
  terminal.
- The printer columns are `Target`, `Phase`, `Trigger`, `PR`, `Age`, and, at
  `-o wide`, `Kind` and `Namespace`.
- `Triage` and `TriageApproval` are removed with no alias, no conversion, and no
  migration. The upgrade guide states the break.

## Consequences

- A recreated object is a new failure: the UID is part of the Fingerprint.
- The CLI is `kubectl page`, and every documented name, metric, and dashboard
  panel uses Page vocabulary.
- v1 objects are not readable by v2 and are not converted. Operators start with
  no Pages after the upgrade.
- Status remains written only by `pagerless-server` (ADR-0001), so the API
  server cannot tell a human from the server by identity alone; decisions are
  reconciled from annotations (ADR-0031).

## Alternatives considered

- **Keep `Triage` and alias `Page`.** A CRD cannot serve two kinds from one
  resource, and shipping both would make every code path branch on a vocabulary
  the product has retired.
- **Convert v1 objects in a migration job.** v1 Pages carry an action-menu
  Proposal and a Workload reference; neither maps onto a Target with a
  Fingerprint, so a conversion would invent data.
