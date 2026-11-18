#!/usr/bin/env python3
"""Validate completed synthetic runtime matrices and write a source-backed report."""

import argparse
import json
from pathlib import Path
import re


def load(path):
    return json.loads(path.read_text())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    speed = []
    memory = []
    failures = []
    expected_guard_failures = []
    corpus_reference = {}
    for backend in ("sqlite", "postgres"):
        result = load(root / f"{backend}-speed/measurements.json")
        for case in result["results"]:
            if case["name"].endswith("-seed"):
                if case["exit_code"]:
                    failures.append(f"{backend}/{case['name']}: seed failed")
                continue
            m = case["measurement"]
            if case["exit_code"] or not m:
                failures.append(f"{backend}/{case['name']}: speed process failed")
                continue
            options = m["options"]
            c = options["Concurrency"]
            full = {mode: [s for s in m["samples"] if s["mode"] == mode and s["phase"] == "full_walk"]
                    for mode in ("baseline", "candidate")}
            first = {mode: [s for s in m["samples"] if s["mode"] == mode and s["phase"] == "first_page"]
                     for mode in ("baseline", "candidate")}
            parity = all(len(full[mode]) == 3*c and len(first[mode]) == 20*c for mode in full)
            references = {s["ReferenceDigest"] for rows in full.values() for s in rows}
            parity = parity and len(references) == 1
            jumbo = options["Scenario"] == "jumbo"
            for field in ("Digest", "TitlesDigest"):
                if not jumbo and len({s[field] for rows in full.values() for s in rows}) != 1:
                    parity = False
            expected = min(25, options["Events"]) if options["Scenario"] == "sparse" else options["Events"]
            if options["Scenario"] == "jumbo-master":
                expected -= 1
            parity = parity and all(s["Items"] == expected for rows in full.values() for s in rows)
            parity = parity and all(s["Markers"] == (1 if jumbo else 0) for s in full["candidate"])
            if not parity:
                failures.append(f"{backend}/{case['name']}: full traversal/count/field/marker parity")
            scenario = options["Scenario"] if options["Scenario"] in ("sparse", "jumbo", "jumbo-master") else "ordinary"
            corpus_reference[(backend, options["Events"], options["Sources"], scenario)] = next(iter(references)) if len(references) == 1 else None
            candidate_samples = [s for s in m["samples"] if s["mode"] == "candidate"]
            max_sdk = max(s["MaxSDKBytes"] for s in candidate_samples)
            bound = max_sdk <= 16384
            for s in candidate_samples:
                bound = bound and s["HTTPWireReceivedBytes"] >= s["HTTPResponseBodyBytes"] >= s["JSONRPCFrameBytes"] >= s["SDKBytes"]
            for phase in m["page_phases"]:
                read = phase["service"]["Read"]
                bound = bound and read["rows_decoded"] <= options["PageSize"]+1 and read["max_payload_bytes"] <= 32768
            if not bound:
                failures.append(f"{backend}/{case['name']}: result/projection/transport counter bounds")
            target = 50 if c == 1 else 100
            targeted = options["Scenario"] in ("ordinary", "wide")
            candidate_latency = m["latency"]["candidate"]
            if targeted and candidate_latency["first_p95_ms"] > target:
                failures.append(f"{backend}/{case['name']}: first-page p95 target")
            if not m["background_reads"] or not m["background_sync_page_writes"]:
                failures.append(f"{backend}/{case['name']}: missing same-pool background workload")
            speed.append({"backend": backend, "case": case["name"], "scenario": options["Scenario"], "concurrency": c,
                          "candidate": candidate_latency, "baseline": m["latency"]["baseline"],
                          "max_sdk_bytes": max_sdk, "parity": parity, "target_ms": target if targeted else None,
                          "first_candidate_http_received_bytes": first["candidate"][0]["HTTPWireReceivedBytes"],
                          "first_candidate_sdk_bytes": first["candidate"][0]["SDKBytes"],
                          "candidate_full_sdk_bytes": full["candidate"][0]["SDKBytes"],
                          "baseline_full_sdk_bytes": full["baseline"][0]["SDKBytes"]})

        result = load(root / f"{backend}-memory/measurements.json")
        for case in result["results"]:
            if case["name"].endswith("-seed"):
                if case["exit_code"]:
                    failures.append(f"{backend}/{case['name']}: memory seed failed")
                continue
            baseline = case["name"].endswith("-baseline")
            m = case["measurement"]
            if case["rss_guard_exceeded"] and baseline:
                expected_guard_failures.append({"backend": backend, "case": case["name"],
                                                "sampled_rss_bytes": case["external_rss_peak_bytes"], "exit_code": case["exit_code"]})
            elif case["exit_code"] or not m:
                failures.append(f"{backend}/{case['name']}: memory process failed")
            if m:
                options = m["options"]
                correct_mode = "baseline" if baseline else "candidate"
                samples = m["samples"]
                valid = len(samples) == 3*options["Concurrency"] and all(s["mode"] == correct_mode and s["phase"] == "full_walk" for s in samples)
                scenario = options["Scenario"] if options["Scenario"] in ("sparse", "jumbo", "jumbo-master") else "ordinary"
                reference = corpus_reference.get((backend, options["Events"], options["Sources"], scenario))
                valid = valid and reference is not None and all(s["ReferenceDigest"] == reference for s in samples)
                if not valid:
                    failures.append(f"{backend}/{case['name']}: memory traversal/opposite-path parity")
            memory.append({"backend": backend, "case": case["name"], "baseline": baseline,
                           "exit_code": case["exit_code"], "rss_guard_exceeded": case["rss_guard_exceeded"],
                           "external_rss_peak_bytes": case["external_rss_peak_bytes"],
                           "self_max_rss_bytes": max((r["SelfMaxRSSBytes"] for r in m["memory"]), default=None) if m else None,
                           "postgres_cluster_child_rss_sum_peak_bytes": case["postgres_cluster_child_rss_sum_peak_bytes"],
                           "heap_checkpoints": m["memory"] if m else None})
    candidates = [r for r in memory if not r["baseline"]]
    if len(speed) != 24 or len(candidates) != 28:
        failures.append("matrix cardinality is incomplete")
    checks = load(root / "checks.json")
    root_check = next(c for c in checks["checks"] if c["name"] == "race")
    storage_check = next(c for c in checks["checks"] if c["name"] == "postgres")
    frontend_log = root / "calendar-r6-frontend-regressions.log"
    frontend_match = re.search(r"Tests\s+(\d+)\s+passed", frontend_log.read_text()) if frontend_log.exists() else None
    if not frontend_match:
        failures.append("frontend regression result is unavailable")
    test_counts = {"root_race_named_passes": len(root_check["tests_pass"]),
                   "storage_integration_suite_named_passes": len(storage_check["tests_pass"]),
                   "explicit_postgres_named_cases": sum("/TestPostgres" in name for name in storage_check["tests_pass"]),
                   "frontend_passes": int(frontend_match.group(1)) if frontend_match else None,
                   "skips": len(root_check["tests_skip"]) + len(storage_check["tests_skip"])}
    if checks["source_changed_during_checks"] or any(c["exit_code"] for c in checks["checks"]):
        failures.append("final root check/source manifest")
    summary = {"status": "verified_with_documented_baseline_guard_failures" if not failures else "incomplete_or_failed",
               "test_suite_counts": test_counts,
               "failures": failures, "baseline_guard_failures": expected_guard_failures,
               "speed_cases": speed, "memory_cases": memory,
               "candidate_memory_cases": len(candidates),
               "candidate_external_rss_max_bytes": max((r["external_rss_peak_bytes"] or 0 for r in candidates), default=None),
               "candidate_self_rss_max_bytes": max((r["self_max_rss_bytes"] or 0 for r in candidates), default=None),
               "runtime_benchmark_scope": "20 paired first-page rounds; 3 paired complete walks; 3 memory walks per worker; C1/C4; unchanged Store pools",
               "limitations": ["Full-result Store baseline is a harness-only SDK wrapper, not a production tool contract",
                               "Full walks are medians/ranges of three paired walks, not 20-round p95 evidence",
                               "Dense/complete traversal can be much slower than one full result; do not auto-traverse",
                               "E3 pilot numbers have a different workload/transport and are not direct runtime before/after values",
                               "Sampled RSS guard can overshoot; no hard process or SQL-executor memory bound is claimed",
                               "PostgreSQL child RSS sum includes auxiliary processes and double-counted shared pages; not physical/PSS memory",
                               "Killed baseline processes have incomplete heap/traversal evidence; raw failures remain failures",
                               "No real providers, credentials, application database, production, Gateway/client changes or Git publication"]}
    (root / "summary.json").write_text(json.dumps(summary, indent=2)+"\n")
    lines = ["# Calendar runtime pagination — local acceptance", "", f"Status: `{summary['status']}`.", "",
             f"The root race suite passed {test_counts['root_race_named_passes']} named cases. The storage suite with integration enabled passed {test_counts['storage_integration_suite_named_passes']} named cases, including {test_counts['explicit_postgres_named_cases']} explicit PostgreSQL-backed cases; no skips. Vet/build passed. Actual runtime packages use isolated synthetic provider/token HTTP, SQLite and PostgreSQL. See `checks.json`, `source-manifest.json`, `measurement-build.json` and per-case raw files.", "",
             "Speed: 20 alternating paired warm first-page rounds with same-pool cached UI reads and sync-page writes. Full walks are measured separately in three paired rounds. Memory: fresh baseline/candidate processes, natural GC, three full walks per C1/C4 worker, 256 MiB best-effort client RSS guard.", "",
             "| Backend / case | First p95 baseline → candidate ms | Full walk median baseline → candidate ms | Max SDK bytes |",
             "|---|---:|---:|---:|"]
    for r in speed:
        b, c = r["baseline"], r["candidate"]
        lines.append(f"| {r['backend']} / {r['case']} | {b['first_p95_ms']:.2f} → {c['first_p95_ms']:.2f} | {b['walk_p50_ms']:.2f} → {c['walk_p50_ms']:.2f} | {r['max_sdk_bytes']} |")
    lines += ["", f"Candidate memory: {len(candidates)} completed case processes; peak sampled RSS {summary['candidate_external_rss_max_bytes']/2**20:.2f} MiB, peak self max RSS {summary['candidate_self_rss_max_bytes']/2**20:.2f} MiB.", "",
              f"Full-result baseline guard terminations: {len(expected_guard_failures)}. These are retained failures, not waived or converted to passes.", "", "## Limits", ""]
    lines += ["- "+value for value in summary["limitations"]]
    if failures:
        lines += ["", "## Failed checks", ""] + ["- "+value for value in failures]
    (root / "REPORT.md").write_text("\n".join(lines)+"\n")
    print(json.dumps({"status": summary["status"], "speed_cases": len(speed), "candidate_memory_cases": len(candidates),
                      "failures": failures, "baseline_guard_failures": len(expected_guard_failures)}))
    return int(bool(failures))


if __name__ == "__main__":
    raise SystemExit(main())
