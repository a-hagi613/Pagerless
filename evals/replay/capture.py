"""Capture and retrieve genuine Replay artifacts from an existing kind stack.

The stages produce the OOMKilled investigation recording and the Narrative
recording. Run with the agent's pinned Python environment. These helpers do not
start a cluster, modify the remote Sandbox, or call a model. The coordinator
starts one live Triage only after the local stack and durable destination are
ready.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import re
import shlex
import subprocess
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from pagerless_agent.budget import MAX_INPUT_TOKENS, MAX_SECONDS, MAX_TOOL_CALLS
from pagerless_agent.contract import PROPOSED_ACTION, DiagnosisOutput, normalize_proposal
from pagerless_agent.identifiers import ReplayError
from pagerless_agent.model import Stage
from pagerless_agent.providers import ANTHROPIC_MODEL
from pagerless_agent.replay import Recording

NAMESPACE = "pagerless"
CLAIM = "pagerless-replay-capture"
CONTAINER_FILE = "/recordings/oom.json"
NARRATIVE_CONTAINER_FILE = "/recordings/narrative.json"
NARRATIVE_SCENARIO = "narrative"
_MAX_ARTIFACT_BYTES = 1 << 20
_TERMINAL = {
    "Recovered",
    "NotRecovered",
    "Diagnosed",
    "Rejected",
    "Expired",
    "Invalid",
    "Failed",
}


def read_api_key(path: Path) -> str:
    """Read the ignored .env as data, without executing shell expansion or displaying its value."""
    for line in path.read_text(encoding="utf-8").splitlines():
        name, separator, raw = line.strip().partition("=")
        if separator and name == "ANTHROPIC_API_KEY":
            values = shlex.split(raw, comments=True, posix=True)
            if (
                len(values) == 1
                and values[0]
                and not any(c.isspace() for c in values[0])
            ):
                return values[0]
    raise RuntimeError("the ignored configuration has no nonempty ANTHROPIC_API_KEY")


class Cluster:
    """A context-explicit subprocess boundary that never puts credentials in argv or diagnostics."""

    def __init__(self, context: str) -> None:
        if not context.startswith("kind-"):
            raise RuntimeError(
                "capture tooling is limited to an explicitly selected kind context"
            )
        self.context = context

    def kubectl(self, *arguments: str, body: dict[str, Any] | None = None) -> bytes:
        return command(
            ["kubectl", "--context", self.context, *arguments],
            json.dumps(body).encode() if body is not None else None,
        )

    def get(
        self, kind: str, name: str | None = None, namespace: str = NAMESPACE
    ) -> dict[str, Any]:
        arguments = ["get", kind, "-o", "json"]
        if namespace:
            arguments += ["-n", namespace]
        if name:
            arguments.append(name)
        return json.loads(self.kubectl(*arguments))  # type: ignore[no-any-return]

    def apply(self, body: dict[str, Any]) -> None:
        self.kubectl(
            "apply",
            "--server-side",
            "--field-manager=pagerless-replay-capture",
            "-f",
            "-",
            body=body,
        )


def command(arguments: list[str], body: bytes | None = None) -> bytes:
    result = subprocess.run(
        arguments,
        input=body,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        # Do not quote argv, stdin, or stderr: server errors may echo request bodies.
        raise RuntimeError(
            f"{Path(arguments[0]).name} command failed with exit code {result.returncode}"
        )
    return result.stdout


def prepare(cluster: Cluster, key_file: Path) -> None:
    """Install the inference key through stdin and create durable local capture storage."""
    key = read_api_key(key_file)
    cluster.apply(
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": "pagerless-agent-credentials", "namespace": NAMESPACE},
            "type": "Opaque",
            "stringData": {"ANTHROPIC_API_KEY": key},
        }
    )
    nodes = cluster.get("nodes", namespace="")["items"]
    if len(nodes) != 1:
        raise RuntimeError(
            "the local capture volume requires a single-node kind cluster"
        )
    node = nodes[0]["metadata"]["name"]
    directory = "/var/local/pagerless-replay-capture"
    command(["docker", "exec", node, "mkdir", "-p", directory])
    command(["docker", "exec", node, "chown", "65532:65532", directory])
    cluster.apply(
        {
            "apiVersion": "v1",
            "kind": "PersistentVolume",
            "metadata": {"name": CLAIM},
            "spec": {
                "capacity": {"storage": "64Mi"},
                "accessModes": ["ReadWriteOnce"],
                "persistentVolumeReclaimPolicy": "Retain",
                "storageClassName": "",
                "claimRef": {"name": CLAIM, "namespace": NAMESPACE},
                "hostPath": {"path": directory, "type": "DirectoryOrCreate"},
                "nodeAffinity": {
                    "required": {
                        "nodeSelectorTerms": [
                            {
                                "matchExpressions": [
                                    {
                                        "key": "kubernetes.io/hostname",
                                        "operator": "In",
                                        "values": [node],
                                    },
                                ]
                            }
                        ]
                    }
                },
            },
        }
    )
    cluster.apply(
        {
            "apiVersion": "v1",
            "kind": "PersistentVolumeClaim",
            "metadata": {"name": CLAIM, "namespace": NAMESPACE},
            "spec": {
                "accessModes": ["ReadWriteOnce"],
                "storageClassName": "",
                "volumeName": CLAIM,
                "resources": {"requests": {"storage": "64Mi"}},
            },
        }
    )
    cluster.kubectl(
        "wait",
        "pvc",
        CLAIM,
        "-n",
        NAMESPACE,
        "--for=jsonpath={.status.phase}=Bound",
        "--timeout=60s",
    )


def validate_recording(path: Path, secrets: tuple[str, ...] = ()) -> Recording:
    """Require a normalized live PatchResources investigation within the configured budgets."""
    payload = path.read_bytes()
    if len(payload) > _MAX_ARTIFACT_BYTES:
        raise RuntimeError("the recording exceeds the capture size limit")
    for secret in secrets:
        if secret and (
            secret.encode() in payload or base64.b64encode(secret.encode()) in payload
        ):
            raise RuntimeError(
                "the recording contains a credential and must not be published"
            )
    recording = Recording.load(path)
    if (recording.model, recording.scenario, recording.workload) != (
        ANTHROPIC_MODEL,
        "oom",
        "checkout",
    ):
        raise RuntimeError(
            "the artifact is not a live Haiku OOMKilled recording for checkout"
        )
    if not recording.turns or recording.turns[0].stage != Stage.INVESTIGATE:
        raise RuntimeError("the recording does not begin with investigation")
    diagnoses = [t for t in recording.turns if t.stage == Stage.DIAGNOSE]
    proposals = [t for t in recording.turns if t.stage == Stage.PROPOSE]
    if len(diagnoses) != 1 or not proposals:
        raise RuntimeError("the recording has no complete Diagnosis and Proposal")
    DiagnosisOutput.model_validate(diagnoses[0].response.structured)
    proposal = PROPOSED_ACTION.validate_python(normalize_proposal(proposals[-1].response.structured))
    if proposal.action != "PatchResources":
        raise RuntimeError(
            "the live OOMKilled investigation did not propose PatchResources"
        )
    tokens = sum(t.response.usage.input_tokens for t in recording.turns)
    calls = sum(
        c.name != "record_finding"
        for t in recording.turns
        for c in t.response.tool_calls
    )
    if tokens > MAX_INPUT_TOKENS or calls > MAX_TOOL_CALLS:
        raise RuntimeError("the live recording exceeds an investigation budget")
    return recording


def validate_narrative_recording(path: Path, secrets: tuple[str, ...] = ()) -> Recording:
    """Require exactly one live Narrative call that is identifier-free."""
    payload = path.read_bytes()
    if len(payload) > _MAX_ARTIFACT_BYTES:
        raise RuntimeError("the recording exceeds the capture size limit")
    for secret in secrets:
        if secret and (
            secret.encode() in payload or base64.b64encode(secret.encode()) in payload
        ):
            raise RuntimeError(
                "the recording contains a credential and must not be published"
            )
    recording = Recording.load(path)
    if (recording.model, recording.scenario) != (ANTHROPIC_MODEL, NARRATIVE_SCENARIO):
        raise RuntimeError("the artifact is not a live Haiku Narrative recording")
    if len(recording.turns) != 1 or recording.turns[0].stage != Stage.NARRATIVE:
        raise RuntimeError("the Narrative recording must hold exactly one call")
    if not recording.turns[0].response.text.strip():
        raise RuntimeError("the live Narrative is empty")
    return recording


def volume_command(
    cluster: Cluster, image: str, program: str, *, read_only: bool
) -> bytes:
    """Run an unprivileged storage probe or retrieval without mounting a service account token."""
    helper = "pagerless-replay-read-" + uuid.uuid4().hex[:8]
    cluster.apply(
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {"name": helper, "namespace": NAMESPACE},
            "spec": {
                "restartPolicy": "Never",
                "automountServiceAccountToken": False,
                "securityContext": {
                    "runAsNonRoot": True,
                    "runAsUser": 65532,
                    "runAsGroup": 65532,
                    "fsGroup": 65532,
                    "seccompProfile": {"type": "RuntimeDefault"},
                },
                "containers": [
                    {
                        "name": "read",
                        "image": image,
                        "imagePullPolicy": "IfNotPresent",
                        "command": [
                            "python",
                            "-c",
                            program,
                        ],
                        "securityContext": {
                            "allowPrivilegeEscalation": False,
                            "readOnlyRootFilesystem": True,
                            "capabilities": {"drop": ["ALL"]},
                        },
                        "resources": {
                            "requests": {"cpu": "10m", "memory": "32Mi"},
                            "limits": {"memory": "128Mi"},
                        },
                        "volumeMounts": [
                            {
                                "name": "recording",
                                "mountPath": "/recordings",
                                "readOnly": read_only,
                            }
                        ],
                    }
                ],
                "volumes": [
                    {
                        "name": "recording",
                        "persistentVolumeClaim": {
                            "claimName": CLAIM,
                            "readOnly": read_only,
                        },
                    }
                ],
            },
        }
    )
    try:
        cluster.kubectl(
            "wait",
            "pod",
            helper,
            "-n",
            NAMESPACE,
            "--for=jsonpath={.status.phase}=Succeeded",
            "--timeout=90s",
        )
        return cluster.kubectl("logs", helper, "-n", NAMESPACE, "-c", "read")
    finally:
        cluster.kubectl(
            "delete",
            "pod",
            helper,
            "-n",
            NAMESPACE,
            "--ignore-not-found",
            "--wait=false",
        )


def probe_storage(cluster: Cluster, image: str) -> None:
    """Verify actual PVC write/read permission before any paid investigation can begin."""
    path = "/recordings/.capture-probe-" + uuid.uuid4().hex
    volume_command(
        cluster,
        image,
        f"from pathlib import Path; p=Path({path!r}); p.write_text('capture-probe'); "
        "assert p.read_text() == 'capture-probe'; p.unlink()",
        read_only=False,
    )


def retrieve(
    cluster: Cluster,
    image: str,
    destination: Path,
    key_file: Path,
) -> Recording:
    """Read and validate the PVC artifact after the original agent container has exited."""
    payload = volume_command(
        cluster,
        image,
        "from pathlib import Path; import sys; p=Path('/recordings/oom.json'); "
        "assert p.stat().st_size <= 1048576; sys.stdout.buffer.write(p.read_bytes())",
        read_only=True,
    )
    temporary = destination.with_suffix(".capture.tmp")
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
        temporary.write_bytes(payload)
        recording = validate_recording(temporary, (read_api_key(key_file),))
        temporary.replace(destination)
        return recording
    finally:
        temporary.unlink(missing_ok=True)


def retrieve_narrative(
    cluster: Cluster,
    image: str,
    destination: Path,
    key_file: Path,
) -> Recording:
    """Read and validate the Narrative artifact after its Job has exited."""
    payload = volume_command(
        cluster,
        image,
        f"from pathlib import Path; import sys; p=Path({NARRATIVE_CONTAINER_FILE!r}); "
        "assert p.stat().st_size <= 1048576; sys.stdout.buffer.write(p.read_bytes())",
        read_only=True,
    )
    temporary = destination.with_suffix(".capture.tmp")
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
        temporary.write_bytes(payload)
        recording = validate_narrative_recording(
            temporary, (read_api_key(key_file),)
        )
        temporary.replace(destination)
        return recording
    finally:
        temporary.unlink(missing_ok=True)


def write_provenance(
    cluster: Cluster, triage: str, artifact: Path, revision: str
) -> None:
    """Bind the artifact digest to the live Job and the server-validated Proposal."""
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise RuntimeError("source revision must be the full Git commit SHA")
    tr = cluster.get("triage", triage)
    job = cluster.get("job", triage + "-agent")
    if (
        tr.get("status", {}).get("phase") != "Proposed"
        or job.get("status", {}).get("succeeded") != 1
    ):
        raise RuntimeError(
            "capture provenance requires a succeeded live Job and a Proposed Triage"
        )
    proposal = tr["status"]["proposal"]
    if proposal["action"] != "PatchResources":
        raise RuntimeError("the server did not validate a PatchResources Proposal")
    container = job["spec"]["template"]["spec"]["containers"][0]
    environment = {entry["name"]: entry.get("value") for entry in container["env"]}
    if environment.get("PAGERLESS_MODEL_PROVIDER") != "anthropic":
        raise RuntimeError("the recorded Job did not use the live Anthropic provider")
    if job["spec"].get("activeDeadlineSeconds") != MAX_SECONDS:
        raise RuntimeError("the live Job did not enforce the five-minute deadline")
    recording = validate_recording(artifact)
    metadata = {
        "format": "pagerless.dev/replay-provenance",
        "version": 1,
        "artifact": artifact.name,
        "sha256": hashlib.sha256(artifact.read_bytes()).hexdigest(),
        "provider": "anthropic",
        "model": recording.model,
        "scenario": recording.scenario,
        "source_revision_at_capture": revision,
        "captured_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "environment": "kind with the isolated local Git/GitHub Sandbox adapter and original read-only MCP servers",
        "triage": {
            "name": tr["metadata"]["name"],
            "uid": tr["metadata"]["uid"],
            "phase": tr["status"]["phase"],
            "proposal_action": proposal["action"],
            "proposal_hash": proposal["hash"],
        },
        "job": {
            "name": job["metadata"]["name"],
            "uid": job["metadata"]["uid"],
            "started_at": job["status"]["startTime"],
            "completed_at": job["status"]["completionTime"],
            "image": container["image"],
            "succeeded": True,
        },
        "usage": {
            "model_calls": len(recording.turns),
            "input_tokens_including_cache": sum(
                t.response.usage.input_tokens for t in recording.turns
            ),
            "output_tokens": sum(
                t.response.usage.output_tokens for t in recording.turns
            ),
        },
        "limits": {
            "tool_calls": MAX_TOOL_CALLS,
            "seconds": MAX_SECONDS,
            "input_tokens": MAX_INPUT_TOKENS,
        },
        "normalization": "Recording.load validates every placeholder and refuses unresolved volatile identifiers",
    }
    artifact.with_suffix(".provenance.json").write_text(
        json.dumps(metadata, indent=2) + "\n", encoding="utf-8"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "stage",
        choices=("prepare", "retrieve", "retrieve-narrative", "validate", "provenance"),
    )
    parser.add_argument("--context", default="kind-pagerless")
    parser.add_argument("--key-file", type=Path, default=Path(".env"))
    parser.add_argument("--artifact", type=Path, default=Path("evals/replay/oom.json"))
    parser.add_argument(
        "--narrative-artifact", type=Path, default=Path("evals/replay/narrative.json")
    )
    parser.add_argument("--image", default="ghcr.io/a-hagi613/pagerless-agent:0.1.0")
    parser.add_argument("--triage")
    parser.add_argument("--source-revision")
    args = parser.parse_args()
    cluster = Cluster(args.context)
    if args.stage == "prepare":
        prepare(cluster, args.key_file)
        probe_storage(cluster, args.image)
        print("Prepared the inference Secret and durable local recording volume.")
    elif args.stage == "retrieve":
        recording = retrieve(cluster, args.image, args.artifact, args.key_file)
        print(
            f"Retrieved and validated {len(recording.turns)} genuine model responses."
        )
    elif args.stage == "retrieve-narrative":
        recording = retrieve_narrative(
            cluster, args.image, args.narrative_artifact, args.key_file
        )
        print(
            f"Retrieved and validated {len(recording.turns)} genuine Narrative response."
        )
    elif args.stage == "provenance":
        if not args.triage or not args.source_revision:
            parser.error("provenance requires --triage and --source-revision")
        write_provenance(cluster, args.triage, args.artifact, args.source_revision)
        print("Recorded the artifact digest and live Job provenance.")
    else:
        recording = validate_recording(args.artifact, (read_api_key(args.key_file),))
        print(
            f"Validated {len(recording.turns)} normalized live model responses; no configured API key found."
        )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (
        OSError,
        RuntimeError,
        ValueError,
        ReplayError,
        KeyError,
        TypeError,
    ) as error:
        # Raw validation errors may include model output or tool-returned data.
        print(
            f"Replay capture stage failed ({type(error).__name__}). No credential or raw response was printed."
        )
        raise SystemExit(1) from None
