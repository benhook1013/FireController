#!/usr/bin/env python3
"""Select complete, disjoint CI slices from Go's actual package/test inventories."""
import argparse
import re
from pathlib import Path

PREFIX = "github.com/benhook1013/FireController/"
FAMILIES = {"backup": 8, "reviewexec": 6, "reviews": 4, "reviewruntime": 2}
REST_SHARDS = 2


def shards():
    counts = {**FAMILIES, "rest": REST_SHARDS}
    return [f"{family}-{index}" for index in range(max(counts.values()))
            for family, count in counts.items() if index < count]


def select_packages(shard, inventory):
    if shard not in shards():
        raise ValueError("unknown Go test shard")
    packages = inventory.splitlines()
    if not packages or len(packages) != len(set(packages)) or any(
        not p.startswith(PREFIX) or not re.fullmatch(r"[\w./-]+", p) for p in packages
    ):
        raise ValueError("invalid or duplicate Go package inventory")
    family, index = shard.rsplit("-", 1)
    if family != "rest":
        package = PREFIX + "internal/" + family
        if package not in packages:
            raise ValueError("partitioned package absent from Go inventory")
        return [package]
    isolated = {PREFIX + "internal/" + name for name in FAMILIES}
    remaining = sorted(set(packages) - isolated, key=lambda name: name.encode("utf-8"))
    selected = remaining[int(index)::REST_SHARDS]
    if not selected:
        raise ValueError("empty remaining-package shard")
    return selected


def select_tests(shard, package, inventory):
    family, index = shard.rsplit("-", 1)
    if family not in FAMILIES or package != PREFIX + "internal/" + family:
        raise ValueError("test partition/package mismatch")
    names = []
    for line in inventory.splitlines():
        if re.fullmatch(r"(?:Test|Example|Fuzz)\w*", line):
            names.append(line)
        elif re.fullmatch(r"Benchmark\w*", line) or re.fullmatch(r"ok\s+" + re.escape(package) + r"\s+.*", line):
            continue
        else:
            raise ValueError("unexpected Go test discovery output")
    names.sort(key=lambda name: name.encode("utf-8"))
    if len(names) != len(set(names)):
        raise ValueError("duplicate Go test name")
    partitions = [names[i::FAMILIES[family]] for i in range(FAMILIES[family])]
    sets = [set(part) for part in partitions]
    if any(not part for part in sets) or set().union(*sets) != set(names) or sum(map(len, sets)) != len(names):
        raise ValueError("incomplete or empty Go test partition")
    return "^(" + "|".join(re.escape(name) for name in partitions[int(index)]) + ")$"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("shard", choices=shards())
    parser.add_argument("packages", type=Path)
    parser.add_argument("--tests", type=Path)
    args = parser.parse_args()
    try:
        selected = select_packages(args.shard, args.packages.read_text(encoding="utf-8"))
        family = args.shard.rsplit("-", 1)[0]
        result = {"packages": " ".join(selected), "partitioned": str(family != "rest").lower()}
        if args.tests:
            if len(selected) != 1:
                raise ValueError("test inventory requires one partitioned package")
            result["run"] = select_tests(args.shard, selected[0], args.tests.read_text(encoding="utf-8"))
        for key, value in result.items():
            print(f"{key}={value}")
    except (ValueError, OSError) as error:
        parser.exit(1, f"CI selection failed: {error}\n")


if __name__ == "__main__":
    main()
