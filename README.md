# Pagerless

Pagerless 1.0 is being rebuilt as a Helm-installed Kubernetes investigation agent. Ask a question in the dashboard or with `kubectl page`, inspect the evidence behind the answer, and review proposed fixes in GitHub.

This branch contains the previous runtime alongside the specification for its replacement. The new distribution is pending. Published legacy artifacts implement the earlier operator.

- [Release specification](docs/specification.md) covers installation, investigation, interfaces and acceptance tests.
- [Architecture decision](docs/adr/0001-investigation-service.md) records the runtime and permission boundaries.
- [Rewrite audit](docs/rewrite.md) lists the removal order, reusable code and planned additions.

Pagerless is maintained in [a-hagi613/pagerless](https://github.com/a-hagi613/pagerless) and distributed under the [Apache License 2.0](LICENSE).
