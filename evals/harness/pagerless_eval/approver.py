"""Provision and use the dedicated eval-approver identity.

The evaluator approves every Proposal as a Kubernetes user called eval-approver,
whose kubeconfig carries only a client certificate for that user. The user is
bound to the existing pagerless-approver Role and nothing else, and the
provisioning checks that the identity cannot write Triage status or cluster RBAC.
The cluster administrator's kubeconfig is used only to create that identity and
its binding; no approval is ever made with it.
"""

from __future__ import annotations

import base64
import hashlib
import json
import subprocess
import time
from pathlib import Path
from typing import Any

from .kube import Kubectl, KubectlError

APPROVER_USER = "eval-approver"
APPROVER_NAMESPACE = "pagerless"
APPROVER_ROLE = "pagerless-approver"
ROLE_BINDING = "pagerless-eval-approver"
CSR_NAME = "pagerless-eval-approver"


class ApproverError(RuntimeError):
    """The eval-approver identity could not be provisioned or verified."""


def approval_name(triage: dict[str, Any], proposal_hash: str) -> str:
    """Return the canonical TriageApproval name, matching api/v1alpha1.

    The digest binds the namespace, name, UID, and Proposal hash, so the same
    Proposal has one contended decision name and a recreated Triage cannot
    reuse an earlier decision.
    """
    metadata = triage.get("metadata") or {}
    identity = "\x00".join(
        [
            str(metadata.get("namespace", "")),
            str(metadata.get("name", "")),
            str(metadata.get("uid", "")),
            proposal_hash,
        ]
    )
    digest = hashlib.sha256(identity.encode()).digest()
    encoded = base64.b32encode(digest).decode("ascii").rstrip("=").lower()
    return "decision-" + encoded


def provision(admin: Kubectl, state_dir: Path) -> Path:
    """Create or renew the eval-approver kubeconfig and return its path."""
    _require_openssl()
    state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    key = state_dir / "eval-approver.key"
    csr = state_dir / "eval-approver.csr"
    certificate = state_dir / "eval-approver.crt"
    kubeconfig = state_dir / "eval-approver.kubeconfig"
    _openssl_request(key, csr)

    admin.run("delete", "csr", CSR_NAME, "--ignore-not-found", timeout=60)
    admin.create(
        {
            "apiVersion": "certificates.k8s.io/v1",
            "kind": "CertificateSigningRequest",
            "metadata": {"name": CSR_NAME},
            "spec": {
                "request": base64.b64encode(csr.read_bytes()).decode("ascii"),
                "signerName": "kubernetes.io/kube-apiserver-client",
                "usages": ["client auth"],
            },
        }
    )
    admin.run("certificate", "approve", CSR_NAME, timeout=60)
    issued = ""
    for _ in range(60):
        issued = admin.run(
            "get", "csr", CSR_NAME, "-o", "jsonpath={.status.certificate}", timeout=30
        ).stdout.strip()
        if issued:
            break
        time.sleep(1)
    if not issued:
        raise ApproverError("the API server did not issue the eval-approver certificate")
    certificate.write_text(base64.b64decode(issued).decode("ascii"), encoding="utf-8")
    certificate.chmod(0o600)
    key.chmod(0o600)

    _write_kubeconfig(admin, kubeconfig, certificate, key)
    _bind_approver_role(admin)
    _verify(kubeconfig)
    return kubeconfig


def create_approval(evaluator: Kubectl, triage: dict[str, Any]) -> str:
    """Create the Approval for the Triage's current Proposal and return its name."""
    status = triage.get("status") or {}
    proposal = status.get("proposal") or {}
    proposal_hash = proposal.get("hash")
    if not isinstance(proposal_hash, str) or len(proposal_hash) != 64:
        raise ApproverError("the Triage has no Proposal hash to approve")
    metadata = triage.get("metadata") or {}
    name = approval_name(triage, proposal_hash)
    document = {
        "apiVersion": "pagerless.dev/v1alpha1",
        "kind": "TriageApproval",
        "metadata": {"name": name, "namespace": APPROVER_NAMESPACE},
        "spec": {
            "triage": metadata.get("name"),
            "proposalHash": proposal_hash,
            "decision": "Approved",
        },
    }
    try:
        evaluator.create(document)
    except KubectlError as error:
        raise ApproverError(
            f"the eval-approver could not create the Approval: {error}. "
            "Delete stale TriageApprovals and retry."
        ) from None
    return name


def _require_openssl() -> None:
    try:
        subprocess.run(["openssl", "version"], capture_output=True, check=True)
    except OSError, subprocess.CalledProcessError:
        raise ApproverError("openssl is required to create the eval-approver certificate") from None


def _openssl_request(key: Path, csr: Path) -> None:
    for path in (key, csr):
        path.unlink(missing_ok=True)
    result = subprocess.run(
        [
            "openssl",
            "req",
            "-new",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(key),
            "-out",
            str(csr),
            "-subj",
            f"/CN={APPROVER_USER}",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode:
        raise ApproverError(f"openssl could not create the eval-approver request: {result.stderr.strip()}")


def _write_kubeconfig(admin: Kubectl, path: Path, certificate: Path, key: Path) -> None:
    view = json.loads(admin.run("config", "view", "--raw", "--minify", "-o", "json").stdout)
    current = view.get("current-context", "")
    context = next((item for item in view.get("contexts", []) if item.get("name") == current), None)
    if context is None:
        raise ApproverError("the administrator kubeconfig has no current context")
    cluster_name = (context.get("context") or {}).get("cluster", "")
    cluster = next((item for item in view.get("clusters", []) if item.get("name") == cluster_name), None)
    if cluster is None:
        raise ApproverError("the administrator kubeconfig has no cluster for its current context")
    details = cluster.get("cluster") or {}
    server = details.get("server")
    authority = details.get("certificate-authority-data")
    if not server or not authority:
        raise ApproverError("the cluster must publish a server and a CA certificate to the kubeconfig")
    document = {
        "apiVersion": "v1",
        "kind": "Config",
        "clusters": [
            {"name": cluster_name, "cluster": {"server": server, "certificate-authority-data": authority}}
        ],
        "users": [
            {
                "name": APPROVER_USER,
                "user": {
                    "client-certificate-data": base64.b64encode(certificate.read_bytes()).decode("ascii"),
                    "client-key-data": base64.b64encode(key.read_bytes()).decode("ascii"),
                },
            }
        ],
        "contexts": [
            {
                "name": APPROVER_USER,
                "context": {"cluster": cluster_name, "user": APPROVER_USER, "namespace": APPROVER_NAMESPACE},
            }
        ],
        "current-context": APPROVER_USER,
    }
    path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def _bind_approver_role(admin: Kubectl) -> None:
    expected = {
        "apiGroup": "rbac.authorization.k8s.io",
        "kind": "Role",
        "name": APPROVER_ROLE,
    }
    subjects = [{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": APPROVER_USER}]
    existing = admin.run(
        "get", "rolebinding", ROLE_BINDING, "-n", APPROVER_NAMESPACE, "-o", "json", check=False
    )
    if existing.returncode == 0:
        binding = json.loads(existing.stdout)
        if binding.get("roleRef") != expected or binding.get("subjects") != subjects:
            raise ApproverError(f"the existing {ROLE_BINDING} RoleBinding differs; refusing to overwrite it")
        return
    admin.create(
        {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "RoleBinding",
            "metadata": {"name": ROLE_BINDING, "namespace": APPROVER_NAMESPACE},
            "roleRef": expected,
            "subjects": subjects,
        }
    )


def _verify(kubeconfig: Path) -> None:
    evaluator = Kubectl(kubeconfig=kubeconfig)
    identity = json.loads(evaluator.run("auth", "whoami", "-o", "json").stdout)
    username = (identity.get("status") or {}).get("userInfo", {}).get("username")
    if username != APPROVER_USER:
        raise ApproverError(f"the eval-approver kubeconfig authenticates as {username!r}")
    if not _can(evaluator, "create", "triageapprovals", namespace=APPROVER_NAMESPACE):
        raise ApproverError("the eval-approver cannot create TriageApprovals")
    if _can(evaluator, "update", "triages/status", namespace=APPROVER_NAMESPACE):
        raise ApproverError("the eval-approver must not be able to write Triage status")
    if _can(evaluator, "create", "clusterrolebindings"):
        raise ApproverError("the eval-approver must not be a cluster administrator")


def _can(evaluator: Kubectl, verb: str, resource: str, namespace: str | None = None) -> bool:
    arguments = ["auth", "can-i", verb, resource]
    if namespace:
        arguments += ["-n", namespace]
    result = evaluator.run(*arguments, check=False)
    return result.stdout.strip() == "yes"
