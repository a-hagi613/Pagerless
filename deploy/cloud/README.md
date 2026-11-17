# Cloud-only manifests

The resources that exist on EKS and have no local counterpart. Everything else in `deploy/` is the same app-of-apps that runs on kind, which is the point: the two environments differ only in the platform under them.

| Path | Applied by | Purpose |
| --- | --- | --- |
| `karpenter/ec2nodeclass.yaml` | `make cloud-up` | Which network, role and AMI Karpenter launches nodes from. |
| `karpenter/nodepool.yaml` | `make cloud-up` | The only capacity Karpenter may buy: x86_64 Spot, within a 12 vCPU and 24 GiB limit. |
| `storage/storage-class.yaml` | `make cloud-up` | The default gp3 class named `standard`, whose volumes carry `project=pagerless`. |

All are plain, static manifests. The values they repeat from Terraform are the node role name (`infra/karpenter.tf`), the discovery tag (`infra/discovery.tf`) and the cluster name. `make cloud-up` applies them only after the Karpenter chart is running, and `make cloud-down` deletes the claims and nodes before it destroys the cluster, so they need no teardown step of their own.
