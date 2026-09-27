#!/usr/bin/env python3
"""Check active container and workflow references for immutable provenance."""

from pathlib import Path
import json
import re

ROOT = Path(__file__).resolve().parent.parent
LOCK = json.loads((ROOT / "hack" / "artifact-lock.json").read_text())


def fail(message: str) -> None:
    raise SystemExit(f"check-provenance: {message}")


expected = set(LOCK.get("base_images", {}).values())
if not expected:
    fail("lock has no base_images")

for name in ("Containerfile", "Containerfile.agent"):
    path = ROOT / name
    found = set()
    for line in path.read_text().splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("FROM "):
            reference = stripped.split()[1]
            if "@sha256:" not in reference:
                fail(f"{name} has an unpinned FROM: {reference}")
            found.add(reference.split(" AS ")[0])
        if re.search(r"/releases/latest(?:/|$)|\b(latest|nightly|master):", stripped, re.I):
            fail(f"{name} contains a mutable active reference: {stripped}")
    if found != expected:
        fail(
            f"{name} base images differ from artifact-lock.json: "
            f"missing={sorted(expected - found)}, extra={sorted(found - expected)}"
        )

workflow = ROOT / ".github/workflows/push-images.yml"
for number, line in enumerate(workflow.read_text().splitlines(), 1):
    if re.search(r"tag=latest|:latest\b", line):
        fail(f"workflow line {number} uses mutable latest")

print("immutable provenance: ok")
