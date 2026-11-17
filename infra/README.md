# Cloud infrastructure

Terraform for the EKS environment that `make cloud-up` creates and `make cloud-down` destroys. The design decisions are [ADR-0009](../docs/adr/0009-eks-network-without-nat-or-load-balancers.md) (no NAT gateways or load balancers), [ADR-0010](../docs/adr/0010-cost-model.md) (near-zero cost), and [ADR-0024](../docs/adr/0024-budget-guardrail-stays-outside-terraform.md) (the budget guardrail stays outside Terraform).

## Layout

```
infra/
  bootstrap/       One-time S3 state bucket, applied with local state
  modules/network/ VPC, public subnets, internet gateway, node security group
  *.tf             The stack: providers, backend, variables, network wiring
```

The stack is split so that a pull request adds one layer at a time: the network here, then the cluster and its compute, then the identity that lets it run the stack.

## The state bucket, once per account

The bucket persists between sessions. Only the cluster is destroyed.

```sh
cd infra/bootstrap
terraform init
terraform apply
```

The bucket is `pagerless-terraform-state-207567775100`, named in `variables.tf` and in `infra/backend.tf`; the two must match. It has versioning, SSE-S3, an ownership control that disables ACLs, a public access block, a lifecycle rule that expires superseded versions after 30 days, and a bucket policy that denies non-TLS access. `force_destroy` is off, so a `terraform destroy` refuses to remove it while it holds state.

## The stack

```sh
cd infra
terraform init
terraform plan
```

Both commands read the account; `plan` changes nothing. The stack refuses to run against any account other than `207567775100` (`allowed_account_ids` in `providers.tf`), and the backend applies the same guard to the state file.

Run it with the `pagerless` profile:

```sh
AWS_PROFILE=pagerless terraform plan
```

## Checks

CI (`.github/workflows/infra.yml`) runs these on every pull request that touches `infra/`, with Terraform 1.17.2, tflint 0.66.0 and the AWS ruleset 0.50.0 pinned, and with no AWS credentials:

```sh
cd infra
terraform fmt -check -recursive
(cd bootstrap && terraform init -backend=false && terraform validate)
terraform init -backend=false && terraform validate
tflint --init && tflint --recursive
```

`terraform init` writes `.terraform.lock.hcl`. The lock file is committed, so every run uses the same provider build.

## Network posture

- Public subnets only, one per Availability Zone, each with a public IPv4 address on launch. Nodes reach Amazon ECR, Bedrock and the cluster endpoint through the internet gateway; there is no NAT gateway.
- No load balancer, no public ingress, and no endpoint other than the EKS API. The dashboard and Grafana are reached with `kubectl port-forward`.
- The node security group has no ingress rule. The control plane reaches the kubelet and webhook ports through the cluster security group that Amazon EKS attaches to every node, which permits traffic only between its own members.

## Cost

Everything in this directory is free to create: a VPC, subnets, an internet gateway, a route table and a security group incur no hourly charge. The first billable resource appears when the cluster is created, which is why the network is applied by `make cloud-up` rather than on its own.

Every resource carries `project=pagerless` through the provider's default tags. The tag is activated as a cost-allocation tag outside Terraform, so the tag value is what Cost Explorer reports per session.

## The budget guardrail is not here

The `pagerless-hard-cap-5usd` budget, its automatic deny action and `PagerlessBudgetsActionRole` are managed by hand and must stay that way. See [ADR-0024](../docs/adr/0024-budget-guardrail-stays-outside-terraform.md). No `aws_budgets_*` resource belongs in this directory.
