#!/usr/bin/env python3
"""Validate and summarize free choices and pinned costs (bytes, not timing)."""
import collections
import csv
import statistics
import sys

with open(sys.argv[1], newline="") as source:
    rows = list(csv.DictReader(source, delimiter="\t"))
states = ("node-churn-v1", "node-hello", "node-empty")


def group(target, arm, mode):
    return [r for r in rows if (r["target"], r["arm"], r["mode"]) == (target, arm, mode)]


def distribution(group):
    counts = collections.Counter(r["node"] for r in group)
    return " / ".join(str(counts[s]) for s in states)


def mean(group, field):
    return f"{statistics.mean(int(r[field]) for r in group):,.1f}"


def pinned(target, arm, field):
    pins = group(target, arm, "pinned")
    assert collections.Counter(r["node"] for r in pins) == collections.Counter(states)
    return " / ".join(f'{int(next(r[field] for r in pins if r["node"] == s)):,}' for s in states)


targets = sorted({r["target"] for r in rows})
for target in targets:
    nix = group(target, "k3snix", "argmin")
    assert len({(r["ties"], r["live_raw"]) for r in nix}) == 1
    assert len(nix) > 0 and len(nix) % 3 == 0
    for arm in ("vanilla-fine", "vanilla-coarse"):
        free = group(target, arm, "free")
        assert len(free) == len(nix)
        assert collections.Counter(r["first_registered"] for r in free) == {
            s: len(nix) // 3 for s in states}
        assert {int(r["run"]) for r in free} == set(range(1, len(nix) + 1))
        pins = group(target, arm, "pinned")
        assert len(pins) == 3
        for r in free:
            cost = next(p for p in pins if p["node"] == r["node"])
            assert (r["live_raw"], r["live_wire"]) == (cost["live_raw"], cost["live_wire"])
for r in rows:
    assert r["model_raw"] == r["live_raw"]

print("C / H / E = churn-v1 / hello / empty states; seed states rotate across fixed worker identities.")
print("Free distribution counts choices; observed means describe the recorded runs per target/style, not a scheduler expectation.")
print("Pinned costs measure each state once per target/style. Raw = NAR / whole-image layer tar bytes.")
print("K3snix ties are listed; the first node alphabetically is copied to.\n")
for field, title in (("live_raw", "Raw bytes"), ("live_wire", "Actual wire bytes (agent uncompressed; registry gzip)")):
    print(f"### {title}\n")
    print("| Target | k3snix choice (ties) | k3snix bytes | Fine free C/H/E | Fine observed mean | Fine pinned C/H/E | Coarse free C/H/E | Coarse observed mean | Coarse pinned C/H/E |")
    print("|---|---|---:|---|---:|---|---|---:|---|")
    for target in targets:
        nix = group(target, "k3snix", "argmin")[0]
        fine = group(target, "vanilla-fine", "free")
        coarse = group(target, "vanilla-coarse", "free")
        ties = nix["ties"].replace("node-", "")
        print(f'| {target} | {ties} | {int(nix[field]):,} | {distribution(fine)} | {mean(fine, field)} | {pinned(target, "vanilla-fine", field)} | {distribution(coarse)} | {mean(coarse, field)} | {pinned(target, "vanilla-coarse", field)} |')
    print()
counts = collections.Counter(r["mode"] for r in rows)
print(f'{len(rows)}/{len(rows)} live measurements matched the model and restored seeded state '
      f'({counts["argmin"]} k3snix, {counts["free"]} free, {counts["pinned"]} pinned).')
