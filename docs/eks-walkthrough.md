# Interactive EKS session walkthrough

This guide covers interactive inspection of a live Amazon EKS session created by `make cloud-up`: opening a shell inside a worker node over AWS Systems Manager Session Manager, driving Triages from the CLI against the `pagerless-eks` context, signing in to the dashboard over Tailscale or localhost, and destroying the session cleanly with `make cloud-down`.

For the Terraform layout, IAM roles, and cost controls that back the cluster, see [`infra/README.md`](../infra/README.md), [ADR-0009](adr/0009-eks-network-without-nat-or-load-balancers.md), [ADR-0010](adr/0010-cost-model.md), and [ADR-0024](adr/0024-budget-guardrail-stays-outside-terraform.md).

## 1. Start or select the EKS session

Create the session (or reuse one that is already up) and point the Pagerless helpers and `kubectl-triage` plugin at the `pagerless-eks` kubeconfig context:

```bash
export AWS_PROFILE=pagerless
export AWS_REGION=us-east-1
export PAGERLESS_KUBE_CONTEXT=pagerless-eks
export PATH="$PWD/bin:$PATH"
```

When you intend to open the dashboard from another machine on a Tailscale tailnet (for example `http://devbox:8080` and `http://devbox:3000`), pass the browser-facing URLs to `make cloud-up` so that `pagerless-server` sets its OIDC redirect base URL and embedded Grafana panel links accordingly:

```bash
PAGERLESS_DASHBOARD_BASE_URL=http://devbox:8080 \
PAGERLESS_GRAFANA_URL=http://devbox:3000 \
AWS_PROFILE=pagerless make cloud-up
```

Verify that the system managed node group, Karpenter nodes, and Argo CD Applications are healthy:

```bash
kubectl --context pagerless-eks get nodes -o wide
kubectl --context pagerless-eks -n argocd get applications
kubectl --context pagerless-eks -n pagerless get pods
kubectl --context pagerless-eks -n sandbox get pods
```

## 2. Open a shell inside an EKS worker node via AWS Systems Manager Session Manager

Both the system node group IAM role (`infra/eks.tf`) and the Karpenter node IAM role (`infra/karpenter.tf`) attach `arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore`. The Amazon Linux 2023 EKS node AMI runs the SSM Agent out of the box and registers over outbound HTTPS, so no SSH key pair and no inbound port-22 security-group rule are created.

List the nodes and their EC2 instance IDs:

```bash
kubectl --context pagerless-eks get nodes \
  -o custom-columns=NAME:.metadata.name,GROUP:.metadata.labels.pagerless\.dev/node-group,POOL:.metadata.labels.karpenter\.sh/nodepool,INSTANCE:.spec.providerID
```

Extract the EC2 instance ID (`i-...`) from the `INSTANCE` column (or select the first Ready node automatically) and open an interactive Session Manager shell:

```bash
INSTANCE_ID="$(kubectl --context pagerless-eks get nodes \
  -o jsonpath='{.items[0].spec.providerID}' | awk -F/ '{print $NF}')"
AWS_PROFILE=pagerless aws ssm start-session --target "$INSTANCE_ID" --region us-east-1
```

Inside the node shell, inspect running containers and the `kubelet` service:

```bash
sudo crictl ps
sudo journalctl -u kubelet -n 50 --no-pager
exit
```

## 3. Run live `kubectl` and `kubectl triage` commands

Build the `kubectl-triage` plugin if `bin/kubectl-triage` is not yet built:

```bash
make build-cli
export PATH="$PWD/bin:$PATH"
```

### Inject a Scenario and follow the Active Triage

Inject the `oom` Scenario into the Sandbox and watch the resulting Triage through `Proposed`:

```bash
export PAGERLESS_KUBE_CONTEXT=pagerless-eks
make break SCENARIO=oom
kubectl triage --context pagerless-eks list
```

Select the newest Triage and stream its live investigation steps, Findings, Diagnosis, and Proposal:

```bash
TRIAGE="$(kubectl triage --context pagerless-eks list | awk 'NR==2 {print $1}')"
kubectl triage --context pagerless-eks show "$TRIAGE" --watch
```

### Start a Triage manually with a Hint

You can also start or join an Active Triage for any Deployment or StatefulSet in an allowlisted Workload namespace and attach a human Hint:

```bash
kubectl triage --context pagerless-eks --namespace sandbox deploy/checkout \
  --hint "Check whether memory-holder was lowered in the latest Sandbox commit."
```

### Approve or reject a Proposal

When the Triage reaches `Proposed`, inspect the rendered change and either record an Approval (bound to the Proposal hash) or record a final Rejection:

```bash
# Review the Findings, Diagnosis, and rendered patch:
kubectl triage --context pagerless-eks show "$TRIAGE"

# Approve the Proposal:
kubectl triage --context pagerless-eks approve "$TRIAGE"

# Or decline the Proposal with a final Rejection reason:
kubectl triage --context pagerless-eks reject "$TRIAGE" \
  --reason "Hold remediation while inspecting node memory pressure."
```

After an Approval, the Executor opens an auto-merging pull request in the Sandbox, Argo CD syncs the merged commit, Verification observes the Workload, and the Narrative Job records a short post-Verification Narrative:

```bash
kubectl --context pagerless-eks -n pagerless get triages "$TRIAGE" -w
kubectl triage --context pagerless-eks show "$TRIAGE"
```

## 4. Open the dashboard via Tailscale (`http://devbox:8080`)

`pagerless-server` serves the dashboard and its Keycloak OIDC proxy on container port `8082` (`svc/pagerless-server` port `8082`).

To forward the dashboard and Grafana on localhost only:

```bash
kubectl --context pagerless-eks -n pagerless port-forward svc/pagerless-server 8080:8082
kubectl --context pagerless-eks -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

To bind the port-forwards on the devbox so a browser on your Tailscale tailnet can reach `http://devbox:8080` and `http://devbox:3000`:

```bash
kubectl --context pagerless-eks -n pagerless port-forward --address 0.0.0.0 svc/pagerless-server 8080:8082
kubectl --context pagerless-eks -n monitoring port-forward --address 0.0.0.0 svc/kube-prometheus-stack-grafana 3000:80
```

If the session was started before setting `PAGERLESS_DASHBOARD_BASE_URL=http://devbox:8080`, update `--dashboard-base-url` and `--grafana-url` on `deployment/pagerless-server` and wait for the rollout:

```bash
kubectl --context pagerless-eks -n argocd patch application root --type=merge \
  -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}'
kubectl --context pagerless-eks -n argocd patch application pagerless-server --type=merge \
  -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}'
kubectl --context pagerless-eks -n pagerless get deployment pagerless-server -o json | \
  jq '(.spec.template.spec.containers[0].args[] | select(startswith("--dashboard-base-url="))) |= "--dashboard-base-url=http://devbox:8080" |
      (.spec.template.spec.containers[0].args[] | select(startswith("--grafana-url="))) |= "--grafana-url=http://devbox:3000"' | \
  kubectl --context pagerless-eks apply -f -
kubectl --context pagerless-eks -n pagerless rollout status deployment/pagerless-server --timeout=180s
```

Read the provisioned operator username and password from Secret `pagerless/pagerless-dashboard-operator` without writing credentials to disk:

```bash
kubectl --context pagerless-eks -n pagerless get secret pagerless-dashboard-operator \
  -o jsonpath='{.data.username}' | base64 --decode; echo
kubectl --context pagerless-eks -n pagerless get secret pagerless-dashboard-operator \
  -o jsonpath='{.data.password}' | base64 --decode; echo
```

Open `http://devbox:8080` (or `http://localhost:8080`), sign in with those credentials, inspect the Triage list and live step stream, start a Triage from `/start`, or approve a Proposal directly from the Triage detail view.

## 5. Clean teardown

When interactive testing is complete, reset the Sandbox if a Scenario is still active and destroy the EKS session:

```bash
export AWS_PROFILE=pagerless
export PAGERLESS_KUBE_CONTEXT=pagerless-eks
make reset
make cloud-down
```

`make cloud-down` removes the Argo CD Applications, drains and deletes Karpenter NodeClaims, deletes workload namespaces and PersistentVolumeClaims, sweeps any tagged or CSI-provisioned EBS volumes, runs `terraform destroy`, deletes the `pagerless-eks` kubeconfig context, and verifies via `hack/cloud-teardown-check.sh` that no billable `project=pagerless` resource remains in the account.
