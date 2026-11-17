#!/usr/bin/env python3
"""Select the named registry reference, never a local alias, after pull-back."""
import json
import re
import sys


def select_reference(repository, references):
    if not isinstance(references, list):
        raise ValueError("Docker did not report registry digests")
    pattern = re.escape(repository) + r"@sha256:[0-9a-f]{64}"
    selected = {value for value in references if isinstance(value, str) and re.fullmatch(pattern, value)}
    if len(selected) != 1:
        raise ValueError("expected one digest for the published registry repository")
    return selected.pop()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: image-reference.py REGISTRY_REPOSITORY < repo-digests.json")
    print(select_reference(sys.argv[1], json.load(sys.stdin)))
