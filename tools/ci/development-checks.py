#!/usr/bin/env python3
"""Reuse backend qualification only for development changes confined to the website."""
import argparse
import re
import subprocess


def website_only(paths):
    return bool(paths) and all(
        path.startswith(("webapp/", "internal/webui/assets/"))
        or path == "docs/browser-experience.md"
        for path in paths
    )


def classify(base, head):
    if not all(re.fullmatch(r"[0-9a-f]{40}", value) for value in (base, head)):
        return False
    try:
        # Missing or unavailable qualification always falls back to the full suite.
        result = subprocess.run(
            ["git", "diff", "--name-only", "--no-renames", "-z", base, head, "--"],
            capture_output=True, check=True, timeout=30,
        )
        paths = result.stdout.decode("utf-8").rstrip("\0").split("\0")
        return website_only(paths)
    except (subprocess.SubprocessError, UnicodeDecodeError):
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="", help="Source of a successful development CI run")
    parser.add_argument("--head", required=True)
    args = parser.parse_args()
    print("website_only=" + str(classify(args.base, args.head)).lower())


if __name__ == "__main__":
    main()
