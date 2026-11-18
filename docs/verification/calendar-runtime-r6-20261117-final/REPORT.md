# Calendar runtime pagination — local acceptance

Status: `verified_with_documented_baseline_guard_failures`.

The root race suite passed 706 named cases. The storage suite with integration enabled passed 97 named cases, including 11 explicit PostgreSQL-backed cases; no skips. Vet/build passed. Actual runtime packages use isolated synthetic provider/token HTTP, SQLite and PostgreSQL. See `checks.json`, `source-manifest.json`, `measurement-build.json` and per-case raw files.

Speed: 20 alternating paired warm first-page rounds with same-pool cached UI reads and sync-page writes. Full walks are measured separately in three paired rounds. Memory: fresh baseline/candidate processes, natural GC, three full walks per C1/C4 worker, 256 MiB best-effort client RSS guard.

| Backend / case | First p95 baseline → candidate ms | Full walk median baseline → candidate ms | Max SDK bytes |
|---|---:|---:|---:|
| sqlite / ordinary25-c1-speed | 14.06 → 9.07 | 19.41 → 116.04 | 15948 |
| sqlite / ordinary25-c4-speed | 24.92 → 28.88 | 28.99 → 313.15 | 15952 |
| sqlite / ordinary50-c1-speed | 13.71 → 9.40 | 18.69 → 116.79 | 15948 |
| sqlite / ordinary50-c4-speed | 37.97 → 30.75 | 27.02 → 314.34 | 15952 |
| sqlite / wide25-s16-c1-speed | 13.86 → 9.83 | 19.08 → 273.30 | 16288 |
| sqlite / wide25-s16-c4-speed | 24.10 → 32.91 | 28.99 → 748.31 | 16288 |
| sqlite / wide50-s16-c1-speed | 14.18 → 10.25 | 19.77 → 281.76 | 16284 |
| sqlite / wide50-s16-c4-speed | 24.09 → 33.75 | 27.41 → 773.67 | 16288 |
| sqlite / dense4000-c1-speed | 142.63 → 86.44 | 183.31 → 9805.32 | 15952 |
| sqlite / sparse4000-c1-speed | 5.17 → 4.70 | 5.08 → 4.38 | 15576 |
| sqlite / jumbo-c1-speed | 15.12 → 9.81 | 20.78 → 120.25 | 15948 |
| sqlite / jumbo-master-c1-speed | 14.08 → 9.35 | 19.41 → 119.53 | 15952 |
| postgres / ordinary25-c1-speed | 11.99 → 3.02 | 18.11 → 36.40 | 15928 |
| postgres / ordinary25-c4-speed | 20.03 → 8.12 | 24.13 → 66.80 | 15928 |
| postgres / ordinary50-c1-speed | 12.38 → 2.94 | 17.67 → 36.53 | 15928 |
| postgres / ordinary50-c4-speed | 20.11 → 9.90 | 25.45 → 76.13 | 15928 |
| postgres / wide25-s16-c1-speed | 12.67 → 3.19 | 17.85 → 92.19 | 16264 |
| postgres / wide25-s16-c4-speed | 19.93 → 10.28 | 23.53 → 190.31 | 16264 |
| postgres / wide50-s16-c1-speed | 12.14 → 3.31 | 17.89 → 94.53 | 16264 |
| postgres / wide50-s16-c4-speed | 20.60 → 10.22 | 23.87 → 201.79 | 16264 |
| postgres / dense4000-c1-speed | 109.86 → 3.77 | 167.53 → 761.22 | 15928 |
| postgres / sparse4000-c1-speed | 1.39 → 2.04 | 1.36 → 1.88 | 15580 |
| postgres / jumbo-c1-speed | 13.81 → 2.75 | 19.55 → 33.39 | 15928 |
| postgres / jumbo-master-c1-speed | 12.57 → 2.66 | 17.76 → 34.71 | 15928 |

Candidate memory: 28 completed case processes; peak sampled RSS 39.23 MiB, peak self max RSS 39.44 MiB.

Full-result baseline guard terminations: 4. These are retained failures, not waived or converted to passes.

## Limits

- Full-result Store baseline is a harness-only SDK wrapper, not a production tool contract
- Full walks are medians/ranges of three paired walks, not 20-round p95 evidence
- Dense/complete traversal can be much slower than one full result; do not auto-traverse
- E3 pilot numbers have a different workload/transport and are not direct runtime before/after values
- Sampled RSS guard can overshoot; no hard process or SQL-executor memory bound is claimed
- PostgreSQL child RSS sum includes auxiliary processes and double-counted shared pages; not physical/PSS memory
- Killed baseline processes have incomplete heap/traversal evidence; raw failures remain failures
- No real providers, credentials, application database, production, Gateway/client changes or Git publication
