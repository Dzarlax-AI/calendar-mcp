# SQLite range and contention — local implementation results

Status: implemented and verified locally. Approved revision1 A+B, with no new caches, indexes, migrations, pool capacity, runtime workers or public API contracts. No commit/push/deployment.

Primary case: actual modern SDK,4000 events /16sources,page_size25,unchanged5ms background full-UI reads and sync writes. The identical fixture is reused across three frozen binaries, with rotated before/middle/after order over3 fresh-process rounds. Before is accepted R7 pagination runtime. Middle contains A (SQL) only; after contains A+B. No acceptance overlays.

Full-walk medians: 14280.34 → 12832.99 → 6031.40 ms; overall 2.37×. Raw per-round ranges are retained. The primary walk emits334pages.

| Case | Before ms | SQL only ms | Both ms | Before/both |
|---|---:|---:|---:|---:|
| events4000-s16-c1-p25-bg1 | 14280.34 | 12832.99 | 6031.40 | 2.37× |
| events400-s1-c1-p25-bg0 | 52.47 | 41.73 | 40.59 | 1.29× |
| events400-s1-c1-p25-bg1 | 101.55 | 79.53 | 58.49 | 1.74× |
| events400-s16-c1-p25-bg0 | 134.50 | 137.38 | 132.37 | 1.02× |
| events400-s16-c1-p25-bg1 | 241.37 | 219.62 | 150.89 | 1.60× |
| events4000-s1-c1-p25-bg0 | 2173.28 | 538.78 | 552.09 | 3.94× |
| events4000-s1-c1-p25-bg1 | 6357.35 | 4723.19 | 900.08 | 7.06× |
| events4000-s16-c1-p25-bg0 | 4947.70 | 3695.58 | 3623.11 | 1.37× |
| events400-s1-c1-p50-bg1 | 90.80 | 80.86 | 58.72 | 1.55× |
| events400-s1-c4-p25-bg1 | 195.72 | 150.41 | 122.18 | 1.60× |
| events400-s1-c4-p50-bg1 | 228.65 | 180.56 | 154.40 | 1.48× |
| events400-s16-c1-p50-bg1 | 220.34 | 208.31 | 156.82 | 1.41× |
| events400-s16-c4-p25-bg1 | 480.81 | 457.81 | 394.85 | 1.22× |
| events400-s16-c4-p50-bg1 | 539.90 | 497.28 | 444.85 | 1.21× |
| jumbo-c1 | 62.03 | 45.51 | 45.49 | 1.36× |
| jumbo-master-c1 | 61.22 | 44.65 | 44.60 | 1.37× |
| memory4000-c4-p50 | 11674.46 | 2126.57 | 2083.55 | 5.60× |

Seventeen main cases preserve counts, item/reference/title digests and the16KiB SDK result limit. Core rounds=3; memory4000/C4 has one round with4workers, not an independent latency distribution. Group-max medians and ranges are in raw summaries. Source clocks/freshness naturally vary; event corpus and target windows stay unchanged.

Additional actual SDK cases: sparse4000 (25 selected events), mixed390 and tied390 with cross-source ID/time ties, masters, stray dates and an oversized marker. Each of3versions walked3times in its fresh process and preserved counts/digests. These are separate static-fixture checks, not the primary background protocol.

## Large payload and concurrency limits

| Actual Store UI C4 / body | Before median ms | Both median ms | Sampled RSS before→both MiB |
|---|---:|---:|---:|
| 512 bytes | 90.37 | 47.37 | 59.34 → 62.84 |
| 2048 bytes | 222.62 | 248.79 | 84.19 → 87.86 |

The one-round4000/16-source background stress with2KiB descriptions measured 25840.90 → 24658.49 → 24470.64 ms. Its UI result exceeds8MiB and falls back to streaming, so B does not provide the primary-case speedup. The standalone large-payload UI/C4 median was about12% slower in this process; this is a documented staging/GC tradeoff, not a universal improvement. No cap was raised to mask it.

UI probe: actual Store.ListCachedEvents,3599events/16sources,one C4warmup and3C4rounds,full EventV2 digest parity. ReadMS excludes hash encoding; process RSS includes it. No forced GC. The bounded raw buffer is8MiB and8192logical rows; result memory, driver allocations and total RSS are not bounded by that number. All256MiB best-effort process guards stayed below threshold. Concurrent UI process peak was87.86MiB; PostgreSQL server RSS was not sampled.

## Checks

Full fresh checker:717 named Go race passes,108 storage named passes with integration, including11 explicit PostgreSQL-backed cases; skips0; vet/build pass; source drift during checks0. The focused buffer tests pass-count=2. Nine SLO cases passed:8SQLite ordinary/wide,page25/50,C1/C4 plus one PostgreSQL16-source control;20warm first-page rounds,limits50/100ms. Full-walk medians from3rounds are not p95.

Negative controls: disabling the row budget fails8193-row path observation; disabling the byte budget fails8388609-byte path observation; replacing native range selection with prior SQL fails the actual-generated-query cursor-bound check. All3expected failures are retained; only temporary Go overlays were used. Acceptance source/binaries remained intact.

Production diff: event_pages.go and event_cache.go only. Shared guarded projection helpers keep the PostgreSQL page path; SQLite adds two global covering range branches and a same-read-snapshot malformed timestamp fallback. SQLite full UI reads close rows before decode when the per-call buffer fits; overflow flushes then permanently streams. No transport/interface/worker layer added. Current lifecycle/cursor/final-check behavior retained.

Implementation: one sequential Terra high storage assignment; coordinator reviewed actual diffs, requested projection reuse and budget-path test refinements, and ran integrated checks/measurements. Test-only context probes observe decoder frames without production hooks; their frame-name dependency is documented in test code.

Temporary PostgreSQL was stopped after checks. Before/middle/final manifests and source archives, exact delta from the dirty baseline, raw measurements, negative controls and hashes are attached in this directory.
