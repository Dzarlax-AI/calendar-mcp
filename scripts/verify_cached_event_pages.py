#!/usr/bin/env python3
"""Run runtime checks with a synthetic environment and preserve raw evidence.

This does not launch the application, discover credentials, or contact providers.
PostgreSQL must be a separately provisioned disposable local database.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import urlparse


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--postgres-url")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    # Do not inherit DATABASE_URL, provider credentials, tokens, proxy settings,
    # or arbitrary Go execution flags from the operator's environment.
    env = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR") if key in os.environ}
    env.update({"LC_ALL": "C", "TZ": "UTC", "GOTOOLCHAIN": "local"})
    if args.postgres_url:
        parsed = urlparse(args.postgres_url)
        if parsed.scheme not in ("postgres", "postgresql") or parsed.hostname not in ("127.0.0.1", "::1"):
            parser.error("PostgreSQL must use an explicit loopback address")
        if not parsed.path.startswith("/calendar_r6_") or parsed.username != "calendar_r6":
            parser.error("PostgreSQL must use the disposable calendar_r6 user/database prefix")
        env["TEST_POSTGRES_URL"] = args.postgres_url

    files = sorted({*root.glob("internal/**/*.go"), *root.glob("internal/storage/migrations/**/*.sql"),
                    *root.glob("cmd/**/*.go"), root / "go.mod", root / "go.sum"})
    manifest = {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest() for path in files}
    (output / "source-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    commands = [
        ("race", ["go", "test", "-race", "-count=1", "-json", "./..."]),
        ("vet", ["go", "vet", "./..."]),
        ("build", ["go", "build", "-o", str(output / "calendar-runtime-pagination-server"), "./cmd/server"]),
    ]
    if args.postgres_url:
        commands.append(("postgres", ["go", "test", "-race", "-tags", "integration", "-count=1", "-json", "./internal/storage"]))
    results = []
    for name, command in commands:
        started = time.monotonic()
        with (output / f"{name}.stdout").open("wb") as stdout, (output / f"{name}.stderr").open("wb") as stderr:
            result = subprocess.run(command, cwd=root, env=env, stdout=stdout, stderr=stderr)
        row = {"name": name, "command": command, "exit_code": result.returncode,
               "elapsed_seconds": time.monotonic() - started}
        if name in ("race", "postgres"):
            events = []
            for line in (output / f"{name}.stdout").read_text().splitlines():
                try:
                    events.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
            for action in ("pass", "fail", "skip"):
                row[f"tests_{action}"] = [f"{e['Package']}/{e['Test']}" for e in events
                                           if e.get("Action") == action and "Test" in e]
        results.append(row)
        print(f"{name}: exit {result.returncode}, {row['elapsed_seconds']:.2f}s", flush=True)
    final_files = sorted({*root.glob("internal/**/*.go"), *root.glob("internal/storage/migrations/**/*.sql"),
                          *root.glob("cmd/**/*.go"), root / "go.mod", root / "go.sum"})
    final = {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest() for path in final_files}
    changed = sorted(key for key in manifest.keys() | final.keys() if manifest.get(key) != final.get(key))
    report = {"scope": "root runtime checks; synthetic environment; no application server launched",
              "postgres": "disposable loopback supplied" if args.postgres_url else "not run",
              "source_changed_during_checks": changed, "checks": results}
    binary = output / "calendar-runtime-pagination-server"
    if binary.is_file():
        report["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
    (output / "checks.json").write_text(json.dumps(report, indent=2) + "\n")
    return int(bool(changed) or any(row["exit_code"] for row in results))


if __name__ == "__main__":
    sys.exit(main())
