#!/usr/bin/env python3
"""Materialize and verify the repository's digest-locked build artifacts."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_LOCK = ROOT / "hack" / "artifact-lock.json"
INVENTORY_NAME = "inventory.json"


def fail(message: str) -> None:
    raise SystemExit(f"prepare-artifacts: {message}")


def digest(path: Path) -> tuple[int, str]:
    hasher = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            size += len(chunk)
            hasher.update(chunk)
    return size, hasher.hexdigest()


def relative_path(value: str, field: str) -> Path:
    path = Path(value)
    if path.is_absolute() or ".." in path.parts or not value:
        fail(f"{field} must be a repository-relative path: {value!r}")
    return path


def validate_lock(lock: dict) -> list[dict]:
    if lock.get("schema") != 1:
        fail("unsupported lock schema")
    artifacts = lock.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        fail("lock must contain at least one artifact")
    seen_names: set[str] = set()
    seen_outputs: set[Path] = set()
    for artifact in artifacts:
        if not isinstance(artifact, dict):
            fail("artifact entries must be objects")
        name = artifact.get("name")
        checksum = artifact.get("sha256")
        output = artifact.get("output")
        if not isinstance(name, str) or not name or name in seen_names:
            fail(f"duplicate or invalid artifact name: {name!r}")
        if not isinstance(checksum, str) or len(checksum) != 64 or any(c not in "0123456789abcdef" for c in checksum):
            fail(f"artifact {name!r} has an invalid sha256")
        output_path = relative_path(output, f"artifact {name!r} output")
        if output_path in seen_outputs:
            fail(f"duplicate artifact output: {output}")
        if output_path == Path(INVENTORY_NAME):
            fail("inventory cannot be an artifact output")
        seen_names.add(name)
        seen_outputs.add(output_path)
        source = artifact.get("source")
        url = artifact.get("url")
        if bool(source) == bool(url):
            fail(f"artifact {name!r} must specify exactly one of source or url")
        if source:
            relative_path(source, f"artifact {name!r} source")
        else:
            parsed = urllib.parse.urlparse(url)
            if parsed.scheme != "https" or not parsed.netloc:
                fail(f"artifact {name!r} URL must be an HTTPS URL")
            if any(part.lower() in {"latest", "main", "master", "nightly"} for part in parsed.path.split("/")):
                fail(f"artifact {name!r} URL is mutable")
    return artifacts


def load(lock_path: Path) -> tuple[dict, list[dict]]:
    try:
        lock = json.loads(lock_path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"read lock {lock_path}: {exc}")
    return lock, validate_lock(lock)


def fetch(artifact: dict, cache_root: Path, offline: bool) -> Path:
    checksum = artifact["sha256"]
    cached = cache_root / checksum
    if cached.is_file() and not cached.is_symlink():
        size, actual = digest(cached)
        if actual == checksum:
            return cached
        if offline:
            fail(f"cached bytes for {artifact['name']!r} do not match {checksum}")
        cached.unlink()
    elif cached.exists():
        fail(f"cache entry is not a regular file: {cached}")

    if offline:
        fail(f"offline artifact is missing from cache: {artifact['name']}")

    source = artifact.get("source")
    if source:
        candidate = ROOT / relative_path(source, "source")
        if not candidate.is_file() or candidate.is_symlink():
            fail(f"source is missing or not a regular file: {candidate}")
        origin = candidate
        copy = True
    else:
        origin = None
        copy = False

    cache_root.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mktemp(prefix=f".{checksum}.", dir=cache_root))
    try:
        if copy:
            shutil.copyfile(origin, temporary)
        else:
            try:
                with urllib.request.urlopen(artifact["url"], timeout=60) as response, temporary.open("wb") as stream:
                    shutil.copyfileobj(response, stream)
            except OSError as exc:
                fail(f"download {artifact['name']!r}: {exc}")
        _, actual = digest(temporary)
        if actual != checksum:
            fail(f"bytes for {artifact['name']!r} have digest {actual}, expected {checksum}")
        temporary.replace(cached)
    finally:
        temporary.unlink(missing_ok=True)
    return cached


def expected_outputs(output_root: Path, artifacts: list[dict]) -> set[Path]:
    return {output_root / relative_path(item["output"], "output") for item in artifacts}


def materialize(lock: dict, artifacts: list[dict], offline: bool) -> None:
    output_root = ROOT / relative_path(lock["output"], "output root")
    cache_root = ROOT / relative_path(lock["cache"], "cache root")
    output_root.mkdir(parents=True, exist_ok=True)
    for artifact in artifacts:
        source = fetch(artifact, cache_root, offline)
        destination = output_root / relative_path(artifact["output"], "output")
        destination.parent.mkdir(parents=True, exist_ok=True)
        temporary = destination.with_name(f".{destination.name}.tmp")
        shutil.copyfile(source, temporary)
        temporary.replace(destination)
    write_inventory(output_root, artifacts)


def write_inventory(output_root: Path, artifacts: list[dict]) -> None:
    entries = []
    for artifact in sorted(artifacts, key=lambda item: item["output"]):
        path = output_root / relative_path(artifact["output"], "output")
        if not path.is_file() or path.is_symlink():
            fail(f"materialized output is not a regular file: {path}")
        size, checksum = digest(path)
        entries.append({"name": artifact["name"], "path": artifact["output"], "size": size, "sha256": checksum})
    inventory = {"schema": 1, "artifacts": entries}
    temporary = output_root / f".{INVENTORY_NAME}.tmp"
    temporary.write_text(json.dumps(inventory, indent=2, sort_keys=True) + "\n")
    temporary.replace(output_root / INVENTORY_NAME)


def check(lock: dict, artifacts: list[dict]) -> None:
    output_root = ROOT / relative_path(lock["output"], "output root")
    expected = expected_outputs(output_root, artifacts)
    actual = {path for path in output_root.rglob("*") if path.is_file() or path.is_symlink()} if output_root.exists() else set()
    inventory = output_root / INVENTORY_NAME
    if actual != expected | {inventory}:
        missing = sorted(str(path.relative_to(output_root)) for path in expected - actual)
        extra = sorted(str(path.relative_to(output_root)) for path in actual - expected - {inventory})
        fail(f"output set differs; missing={missing}, extra={extra}")
    for artifact in artifacts:
        path = output_root / relative_path(artifact["output"], "output")
        if path.is_symlink() or not path.is_file():
            fail(f"output is not a regular file: {path}")
        _, actual_checksum = digest(path)
        if actual_checksum != artifact["sha256"]:
            fail(f"output {artifact['name']!r} has digest {actual_checksum}, expected {artifact['sha256']}")
    try:
        recorded = json.loads(inventory.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"read inventory: {exc}")
    expected_inventory = []
    for artifact in sorted(artifacts, key=lambda item: item["output"]):
        path = output_root / relative_path(artifact["output"], "output")
        size, checksum = digest(path)
        expected_inventory.append({"name": artifact["name"], "path": artifact["output"], "size": size, "sha256": checksum})
    if recorded != {"schema": 1, "artifacts": expected_inventory}:
        fail("inventory does not match output bytes")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("prepare", "check", "offline-recreate"))
    parser.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    args = parser.parse_args()
    lock_path = args.lock if args.lock.is_absolute() else ROOT / args.lock
    lock, artifacts = load(lock_path)
    if args.command == "check":
        check(lock, artifacts)
    else:
        materialize(lock, artifacts, offline=args.command == "offline-recreate")
        check(lock, artifacts)


if __name__ == "__main__":
    main()
