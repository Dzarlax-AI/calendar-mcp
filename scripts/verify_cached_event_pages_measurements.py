#!/usr/bin/env python3
"""Measure synthetic runtime SDK paths; never loads application configuration."""

import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from urllib.parse import urlparse, parse_qsl, urlencode, urlunparse
import uuid


def run(binary, command, output, name, env, guard_mib=None, postgres_pid=None):
    stdout = output / f"{name}.stdout.json"
    stderr = output / f"{name}.stderr"
    samples = []
    postgres_samples = []
    exceeded = False
    started = time.monotonic()
    with stdout.open("wb") as out, stderr.open("wb") as err:
        process = subprocess.Popen([str(binary), *command], env=env, stdout=out, stderr=err)
        while process.poll() is None:
            try:
                value = subprocess.check_output(["ps", "-o", "rss=", "-p", str(process.pid)], env=env, text=True)
                rss = int(value.strip()) * 1024
                samples.append({"seconds": time.monotonic() - started, "rss_bytes": rss})
                if guard_mib and rss > guard_mib * 1024 * 1024:
                    exceeded = True
                    process.terminate()
                    break
            except (ValueError, subprocess.CalledProcessError):
                pass
            if postgres_pid:
                try:
                    children = subprocess.check_output(["pgrep", "-P", str(postgres_pid)], env=env, text=True).split()
                    values = subprocess.check_output(["ps", "-o", "rss=", "-p", ",".join(children)], env=env, text=True).split()
                    postgres_samples.append({"seconds": time.monotonic() - started,
                                             "cluster_child_rss_sum_bytes": sum(int(value) * 1024 for value in values),
                                             "processes": len(values)})
                except (ValueError, subprocess.CalledProcessError):
                    pass
            time.sleep(0.02)
        try:
            code = process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            code = process.wait()
    (output / f"{name}.rss.json").write_text(json.dumps(samples) + "\n")
    (output / f"{name}.postgres-rss.json").write_text(json.dumps(postgres_samples) + "\n")
    result = {"name": name, "command": command, "exit_code": code,
              "elapsed_seconds": time.monotonic() - started,
              "external_rss_peak_bytes": max((row["rss_bytes"] for row in samples), default=None),
              "rss_samples": len(samples), "rss_guard_exceeded": exceeded}
    result["postgres_cluster_child_rss_sum_peak_bytes"] = max((row["cluster_child_rss_sum_bytes"] for row in postgres_samples), default=None)
    try:
        result["measurement"] = json.loads(stdout.read_text())
    except (ValueError, OSError):
        result["measurement"] = None
    print(f"{name}: exit {code}, RSS {result['external_rss_peak_bytes']}, {result['elapsed_seconds']:.2f}s", flush=True)
    return result


@contextmanager
def fixture_database(postgres_base, env):
    if not postgres_base:
        with tempfile.TemporaryDirectory(prefix="calendar-r6-fixture-", dir="/private/tmp") as fixture:
            yield "sqlite://" + str(Path(fixture) / "cache.db")
        return
    parsed = urlparse(postgres_base)
    schema = "r6_measure_" + uuid.uuid4().hex
    subprocess.run(["psql", "-X", "--set", "ON_ERROR_STOP=1", "--dbname", postgres_base,
                    "-c", "CREATE SCHEMA " + schema], env=env, check=True, capture_output=True)
    try:
        query = dict(parse_qsl(parsed.query))
        query["search_path"] = schema
        yield urlunparse(parsed._replace(query=urlencode(query)))
    finally:
        subprocess.run(["psql", "-X", "--set", "ON_ERROR_STOP=1", "--dbname", postgres_base,
                        "-c", "DROP SCHEMA " + schema + " CASCADE"], env=env, check=True, capture_output=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--mode", choices=["speed", "memory"], required=True)
    parser.add_argument("--postgres-base", help="disposable loopback calendar_r6 database URL; use a fresh DB per invocation")
    parser.add_argument("--postgres-postmaster-pid", type=int, help="postmaster of this task's disposable tmp cluster, for separate child RSS sampling")
    parser.add_argument("--case", help="run a single named case without changing its parameters")
    args = parser.parse_args()
    if args.postgres_base:
        parsed = urlparse(args.postgres_base)
        if parsed.scheme not in ("postgres", "postgresql") or parsed.hostname not in ("127.0.0.1", "::1") or parsed.username != "calendar_r6" or not parsed.path.startswith("/calendar_r6_"):
            parser.error("PostgreSQL must use an explicit disposable loopback calendar_r6 database")
    if args.postgres_postmaster_pid:
        command = subprocess.check_output(["ps", "-p", str(args.postgres_postmaster_pid), "-o", "command="], text=True)
        if "postgres" not in command or "/private/tmp/calendar-r6-" not in command:
            parser.error("RSS sampling is restricted to the disposable calendar-r6 tmp cluster")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    binary = args.binary.resolve()
    env = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR") if key in os.environ}
    env.update(LC_ALL="C", TZ="UTC")
    speed_cases = [
        (f"ordinary{size}-c{c}", "ordinary", 400, 1, c, size)
        for size in (25, 50) for c in (1, 4)
    ] + [
        (f"wide{size}-s16-c{c}", "wide", 400, 16, c, size)
        for size in (25, 50) for c in (1, 4)
    ] + [("dense4000-c1", "dense", 4000, 1, 1, 25), ("sparse4000-c1", "sparse", 4000, 1, 1, 25),
         ("jumbo-c1", "jumbo", 400, 1, 1, 25), ("jumbo-master-c1", "jumbo-master", 400, 1, 1, 25)]
    memory_cases = [
        (f"events{events}-p{size}-c{c}", "ordinary", events, 1, c, size)
        for events in (400, 4000) for size in (25, 50) for c in (1, 4)
    ] + [(f"wide-s16-p{size}-c{c}", "wide", 400, 16, c, size) for size in (25, 50) for c in (1, 4)] + [
         ("jumbo-c1", "jumbo", 400, 1, 1, 25), ("jumbo-master-c1", "jumbo-master", 400, 1, 1, 25)]
    cases = speed_cases if args.mode == "speed" else memory_cases
    if args.case:
        cases = [row for row in cases if row[0] == args.case]
        if not cases:
            parser.error("unknown case")
    results = []
    for name, scenario, count, sources, concurrency, size in cases:
        # Seed runs in another process and exits before either measurement.
        # Memory baseline and candidate use fresh processes against that fixture.
        with fixture_database(args.postgres_base, env) as database:
            common = ["-database-url", database, "-scenario", scenario, "-events", str(count),
                      "-sources", str(sources), "-concurrency", str(concurrency), "-page-size", str(size),
                      "-body-bytes", "2048" if args.mode == "memory" else "512"]
            seeded = run(binary, ["-mode", "seed", *common], output, name + "-seed", env, postgres_pid=args.postgres_postmaster_pid)
            results.append(seeded)
            if seeded["exit_code"]:
                continue
            modes = ["baseline", "candidate"] if args.mode == "memory" else ["speed"]
            for mode in modes:
                command = ["-mode", mode, *common]
                if args.mode == "speed":
                    command += ["-background", "-rounds", "20"]
                else:
                    command += ["-walks", "3"]
                results.append(run(binary, command, output, name + "-" + mode, env,
                                   256 if args.mode == "memory" else None, args.postgres_postmaster_pid))
    report = {"binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "backend": "PostgreSQL unchanged default Store pool" if args.postgres_base else "SQLite unchanged default Store pool",
              "mode": args.mode, "results": results,
              "limitations": ["External RSS sampled best effort at approximately 20ms; overshoot possible",
                              "One process contains actual SDK server and SDK clients",
                              "PostgreSQL cluster child RSS sum, when sampled, includes auxiliary processes and double-counted shared pages; never add it to client RSS",
                              "Baseline is existing full-result Store API through a harness-only SDK wrapper"]}
    (output / "measurements.json").write_text(json.dumps(report, indent=2) + "\n")
    return int(any(row["exit_code"] or row["rss_guard_exceeded"] for row in results))


if __name__ == "__main__":
    raise SystemExit(main())
