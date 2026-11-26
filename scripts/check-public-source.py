#!/usr/bin/env python3
"""Check public source without displaying credential or deployment values."""
import argparse
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import sys


PATTERNS = {
    "private key": rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----",
    "AWS access key": rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b",
    "GitHub token": rb"\b(?:gh[pousr]_[A-Za-z0-9]{20,255}|github_pat_[A-Za-z0-9_]{20,255})\b",
    "provider API key": rb"\b(?:sk-(?:proj-|svcacct-|ant-)?[A-Za-z0-9_-]{20,200}|AIza[A-Za-z0-9_-]{30,40}|xox[baprs]-[A-Za-z0-9-]{20,200})\b",
    "JWT": rb"\beyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b",
}
ACCOUNTS = re.compile(rb"(?<![A-Za-z0-9])\d{12}(?![A-Za-z0-9])")
# Public Canonical AMI owner; all other permitted accounts are synthetic examples.
EXAMPLE_ACCOUNTS = {b"123456789012", b"099720109477"} | {str(i).encode() * 12 for i in range(10)}
RESOURCES = re.compile(rb"\b(?:ami|vpc|subnet|sg|igw|rtb|vol|snap|i)-([0-9a-f]{8,17})\b")
UUIDS = re.compile(rb"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b", re.I)
HOMES = re.compile(rb"(?<![A-Za-z0-9_$}])/(?:Users|home)/([A-Za-z0-9_.-]+)")
EXAMPLE_USERS = {b"fixture", b"runner", b"you", b"example", b"user", b"test-user", b"alice", b"bob"}
RUNTIME_FILES = {"server.json", "worker.json", "workstation.json", "github-queue.json", "credential.json", "credentials", "auth.json"}


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def private_name(name):
    path = PurePosixPath(name.lower())
    if path.name.startswith(".env") and path.name != ".env.example":
        return True
    return (path.name in RUNTIME_FILES
            or bool(re.search(r"\.(?:local\.(?:json|ya?ml)|pem|key|p12|pfx|tfstate(?:\..*)?|tfvars)$", path.name))
            or any(part in {"local", "private"} for part in path.parts[:-1])
            or name.startswith(".github/sluicework") and name.endswith(".json"))


def read_private_references(path):
    """Read literal identifiers without following a symlink or exposing contents."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 65536:
            raise ValueError("private references must be a mode-600 regular file, at most 64 KiB")
        data = source.read(65537)
    if len(data) > 65536:
        raise ValueError("private reference limit exceeded")
    references = []
    for line in data.decode("utf-8").splitlines():
        value = line.strip()
        if value and not value.startswith("#"):
            if len(value) < 4 or "\0" in value:
                raise ValueError("invalid private reference")
            references.append(value.encode().lower())
    return tuple(set(references))


def findings(name, data, mode="100644", private_references=()):
    result = []
    if private_name(name):
        result.append(("private runtime/configuration file", 0))
    if mode == "160000":
        result.append(("unreviewed nested repository", 0))
    if mode == "120000":
        result.append(("symlink requires publication review", 0))
    if name.endswith(".json"):
        try:
            config = json.loads(data)
        except (ValueError, UnicodeDecodeError):
            config = None
        if isinstance(config, dict) and {"backendId", "appId", "installationId", "repositories"} <= config.keys():
            synthetic_ids = all(isinstance(config[key], int) and (0 < config[key] <= 1000 or 100000 <= config[key] <= 100999)
                                for key in ["appId", "installationId"])
            repositories = config["repositories"]
            synthetic_repositories = isinstance(repositories, list) and all(
                isinstance(repo, dict) and re.fullmatch(r"(?:example-org|your-org|example|team|org)/[A-Za-z0-9_.-]+", str(repo.get("name", "")))
                for repo in repositories)
            if not synthetic_ids or not synthetic_repositories:
                result.append(("queue example contains non-example organization/App configuration", 0))
    for category, pattern in PATTERNS.items():
        for match in re.finditer(pattern, data):
            if category == "AWS access key" and b"EXAMPLE" in match.group():
                continue
            result.append((category, data.count(b"\n", 0, match.start()) + 1))
    uuid_spans = [match.span() for match in UUIDS.finditer(data)]
    for match in ACCOUNTS.finditer(data):
        if match.group() not in EXAMPLE_ACCOUNTS and not any(start <= match.start() and match.end() <= end for start, end in uuid_spans):
            result.append(("non-example AWS/account-sized identifier", data.count(b"\n", 0, match.start()) + 1))
    for match in RESOURCES.finditer(data):
        if not re.fullmatch(rb"0{6,}[0-9a-f]{1,3}", match.group(1)):
            result.append(("non-example AWS resource identifier", data.count(b"\n", 0, match.start()) + 1))
    for match in HOMES.finditer(data):
        if match.group(1) not in EXAMPLE_USERS:
            result.append(("personal home path", data.count(b"\n", 0, match.start()) + 1))
    folded = data.lower()
    for reference in private_references:
        index = folded.find(reference)
        if index >= 0:
            result.append(("private organizational/infrastructure reference", data.count(b"\n", 0, index) + 1))
        if reference in name.lower().encode(errors="surrogateescape"):
            result.append(("private organizational/infrastructure filename", 0))
    return result


def tree(root, revision):
    for entry in git(root, "ls-tree", "-rz", revision).split(b"\0"):
        if entry:
            meta, name = entry.split(b"\t", 1)
            mode, kind, oid = meta.decode().split()
            yield name.decode(errors="surrogateescape"), mode, kind, oid


def check(root, history=False, staged=False, private_references=()):
    issues, seen = [], set()

    def inspect(name, data, mode, revision="working tree"):
        for category, line in findings(name, data, mode, private_references):
            issue = (name, category, line, revision)
            if issue not in seen:
                seen.add(issue)
                issues.append(issue)

    if history:
        revisions = git(root, "rev-list", "HEAD").decode().splitlines()
        blobs = set()
        for revision in revisions:
            for name, mode, kind, oid in tree(root, revision):
                identity = (name, mode, oid)
                if identity in blobs:
                    continue
                blobs.add(identity)
                data = git(root, "cat-file", "blob", oid) if kind == "blob" else b""
                inspect(name, data, mode, revision[:12])
    elif staged:
        for entry in git(root, "ls-files", "--stage", "-z").split(b"\0"):
            if not entry:
                continue
            meta, name = entry.split(b"\t", 1)
            mode, oid, stage = meta.decode().split()
            if stage != "0":
                raise ValueError("resolve index conflicts before publication checks")
            inspect(name.decode(errors="surrogateescape"), b"" if mode == "160000" else git(root, "cat-file", "blob", oid), mode, "index")
    else:
        names = set(git(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").split(b"\0"))
        for raw in sorted(names - {b""}):
            name = raw.decode(errors="surrogateescape")
            path = root / name
            if path.is_symlink():
                inspect(name, b"", "120000")  # Never read an external symlink target.
            elif path.is_file():
                inspect(name, path.read_bytes(), "100644")
            elif path.is_dir():
                inspect(name, b"", "160000")
    return issues


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--history", action="store_true", help="inspect every reachable HEAD commit")
    mode.add_argument("--staged", action="store_true", help="inspect index blobs rather than working files")
    parser.add_argument("--private-reference", action="append", default=[], metavar="FILE",
                        help="optional protected literal identifier list, kept outside public source; repeat for multiple organizations")
    args = parser.parse_args()
    try:
        root = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
        references = tuple({value for path in args.private_reference for value in read_private_references(path)})
        issues = check(root, args.history, args.staged, references)
    except (OSError, ValueError, subprocess.CalledProcessError):
        print("Cannot complete public source checks; inspect the repository/index.", file=sys.stderr)
        return 2
    for name, category, line, revision in issues:
        print(f"{json.dumps(name)}:{line}: {category} ({revision}); value omitted")
    if issues:
        print(f"Public source checks failed: {len(issues)} finding(s).", file=sys.stderr)
        return 1
    print("Public source checks passed. Review screenshots and generated artifacts separately.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
