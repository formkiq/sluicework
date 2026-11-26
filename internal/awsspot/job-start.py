#!/usr/bin/python3
"""Gate the one-job runner before any workflow steps receive execution."""
import json
import os
from pathlib import Path


def authorized(env, event, expected):
    job = expected["job"]
    identity = {
        "GITHUB_REPOSITORY": job["repository"],
        "GITHUB_WORKFLOW_REF": job["workflowRef"],
        "GITHUB_SHA": job["workflowSha"],
        "GITHUB_EVENT_NAME": "workflow_dispatch",
    }
    inputs = {
        "request_id": job["requestId"],
        "source_sha": job["sourceSha"],
        "build_kind": job["kind"],
        "host": "local",
        "local_platform": "linux-arm64",
        "machine_label": expected["label"],
    }
    return all(env.get(k) == v for k, v in identity.items()) and all(
        event.get("inputs", {}).get(k) == v for k, v in inputs.items()
    )


if __name__ == "__main__":
    expected = json.loads(Path("/opt/sluicework-runner/expected-job.json").read_text())
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    if not authorized(os.environ, event, expected):
        raise SystemExit("This Spot runner is reserved for a different build")
