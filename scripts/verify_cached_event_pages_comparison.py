#!/usr/bin/env python3
"""Compare two frozen cached-page binaries on isolated, identical fixtures."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
from urllib.parse import urlparse

from verify_cached_event_pages_measurements import fixture_database, run


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", type=Path, required=True)
    parser.add_argument("--after", type=Path, required=True)
    parser.add_argument("--middle", type=Path, help="Optional frozen binary after the SQL-only step")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--postgres-base")
    parser.add_argument("--case", help="Run one named case, preserving its parameters")
    args = parser.parse_args()
    if args.postgres_base:
        parsed = urlparse(args.postgres_base)
        if (parsed.hostname not in ("127.0.0.1", "::1") or parsed.username != "calendar_r6"
                or not parsed.path.startswith("/calendar_r6_")
                or parsed.scheme not in ("postgres", "postgresql")):
            parser.error("Only an explicit disposable loopback calendar_r6 database is allowed")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    binaries = {key: getattr(args, key).resolve() for key in ("before", "after")}
    if args.middle:
        binaries = {"before": binaries["before"], "middle": args.middle.resolve(), "after": binaries["after"]}
    env = {key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ}
    env.update(TMPDIR="/private/tmp", LC_ALL="C", TZ="UTC")
    cases = [
        (f"events{count}-s{sources}-c1-p25-bg{int(bg)}", "ordinary", count, sources, 1, 25, bg, 512, 3)
        for count in (400, 4000) for sources in (1, 16) for bg in (False, True)
    ] + [
        (f"events400-s{sources}-c{concurrency}-p{size}-bg1", "ordinary", 400,
         sources, concurrency, size, True, 512, 3)
        for sources in (1, 16) for concurrency, size in ((1, 50), (4, 25), (4, 50))
    ] + [
        (f"{scenario}-c1", scenario, 400, 1, 1, 25, False, 2048, 3)
        for scenario in ("jumbo", "jumbo-master")
    ] + [("memory4000-c4-p50", "ordinary", 4000, 1, 4, 50, False, 2048, 1)]
    if args.middle:
        cases.sort(key=lambda case: case[0] != "events4000-s16-c1-p25-bg1")
    if args.case:
        cases = [case for case in cases if case[0] == args.case]
        if not cases:
            parser.error("Unknown case")
    results, summaries = [], []
    for index, (name, scenario, count, sources, concurrency, size, bg, body, rounds) in enumerate(cases):
        with fixture_database(args.postgres_base, env) as database:
            common = ["-database-url", database, "-scenario", scenario, "-events", str(count),
                      "-sources", str(sources), "-concurrency", str(concurrency),
                      "-page-size", str(size), "-body-bytes", str(body)]
            seed = run(binaries["before"], ["-mode", "seed", *common], output, name + "-seed", env)
            results.append(seed)
            if seed["exit_code"]:
                raise RuntimeError("Synthetic seed failed")
            expected = None
            grouped = {key: [] for key in binaries}
            # Alternate AB / BA between rounds and invert the first order between cases.
            # Every invocation is a fresh process with natural GC and one complete walk.
            for round_index in range(rounds):
                if args.middle:
                    orders = [("before", "middle", "after"), ("after", "middle", "before"),
                              ("middle", "before", "after")]
                    order = orders[(round_index + index) % len(orders)]
                else:
                    order = ("before", "after") if (round_index + index) % 2 == 0 else ("after", "before")
                for version in order:
                    command = ["-mode", "candidate", *common, "-walks", "1"]
                    if bg:
                        command.append("-background")
                    row = run(binaries[version], command, output,
                              f"{name}-{version}-round{round_index}", env, guard_mib=256)
                    results.append(row)
                    if row["exit_code"] or row["rss_guard_exceeded"] or not row["measurement"]:
                        raise RuntimeError("Comparison process failed; raw evidence retained")
                    measurement = row["measurement"]
                    assert measurement["provider_calls"] == 0 and measurement["pool_tuning"] is False
                    samples = measurement["samples"]
                    assert len(samples) == concurrency
                    for sample in samples:
                        parity = tuple(sample[key] for key in ("Items", "Markers", "Digest", "TitlesDigest", "ReferenceDigest"))
                        expected = parity if expected is None else expected
                        assert parity == expected, (name, "parity mismatch")
                        assert sample["Items"] == count - (scenario == "jumbo-master")
                        assert sample["MaxSDKBytes"] <= 16384
                    grouped[version].append({"round": round_index, "samples": samples,
                                             "peak_rss_bytes": row["external_rss_peak_bytes"],
                                             "background_reads": measurement["background_reads"],
                                             "background_writes": measurement["background_sync_page_writes"]})
            summary = {"name": name, "rounds": rounds, "concurrency": concurrency,
                       "background": bg, "events": count, "sources": sources, "page_size": size,
                       "description_bytes": body, "parity": "verified", "versions": {}}
            for version, records in grouped.items():
                values = [sample["WalkMS"] for record in records for sample in record["samples"]]
                group_values = [max(sample["WalkMS"] for sample in record["samples"]) for record in records]
                summary["versions"][version] = {
                    "worker_walk_median_ms": statistics.median(values), "worker_walk_range_ms": [min(values), max(values)],
                    "group_max_walk_median_ms": statistics.median(group_values),
                    "pages": records[0]["samples"][0]["Pages"],
                    "peak_sampled_rss_bytes": max(record["peak_rss_bytes"] or 0 for record in records),
                    "records": records}
            summary["speedup"] = (summary["versions"]["before"]["worker_walk_median_ms"]
                                  / summary["versions"]["after"]["worker_walk_median_ms"])
            summaries.append(summary)
            (output / "comparison.json").write_text(json.dumps({"cases": summaries, "results": results}, indent=2) + "\n")
    report = {"synthetic_only": True, "new_caches": False, "pool_tuning": False,
              "binaries": {key: {"path": str(path), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
                           for key, path in binaries.items()},
              "backend": "postgres" if args.postgres_base else "sqlite", "cases": summaries,
              "method": "Three fresh-process rounds per C1/C4 case; one round for memory4000. Two binaries alternate AB/BA; optional three binaries rotate before/middle/after, after/middle/before, middle/before/after. Per-worker medians and ranges, not p95. Identical fixture; seed exits first. Natural GC, best-effort RSS sample/256MiB guard. Background is the existing 5ms UI-read/sync-write loop.",
              "limitations": ["No pure network timing; client wall includes SDK validation and hashes.",
                              "Single-round memory4000 is a stress check, not a latency distribution.",
                              "RSS sampling may miss peaks; no hard SQL-executor memory bound."]}
    (output / "comparison-summary.json").write_text(json.dumps(report, indent=2) + "\n")
    print("Completed", len(summaries), "paired cases", flush=True)


if __name__ == "__main__":
    main()
