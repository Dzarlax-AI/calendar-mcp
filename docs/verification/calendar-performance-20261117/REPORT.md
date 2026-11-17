# Calendar cached traversal optimization — local acceptance

Status: implemented and verified locally. User approved revision1 steps A–C. No new caches, indexes, migrations or pool changes. No commit, push or deployment.

The frozen before binary is the current R6 runtime, not the unbounded full-Store baseline. Each pair uses one isolated fixture and alternating AB/BA fresh processes, three complete walks per version (one round for the C4/4000 memory stress). Values are per-worker medians; ranges are in raw summaries. First-page SLO has separate20 warm rounds.

| Backend / case | Before ms | After ms | Speedup |
|---|---:|---:|---:|
| sqlite / events400-s1-c1-p25-bg0 | 88.35 | 57.77 | 1.53× |
| sqlite / events400-s1-c1-p25-bg1 | 122.92 | 86.08 | 1.43× |
| sqlite / events400-s16-c1-p25-bg0 | 200.20 | 129.80 | 1.54× |
| sqlite / events400-s16-c1-p25-bg1 | 275.35 | 198.80 | 1.39× |
| sqlite / events4000-s1-c1-p25-bg0 | 5630.32 | 2103.97 | 2.68× |
| sqlite / events4000-s1-c1-p25-bg1 | 10109.28 | 6262.32 | 1.61× |
| sqlite / events4000-s16-c1-p25-bg0 | 12554.38 | 5156.41 | 2.43× |
| sqlite / events4000-s16-c1-p25-bg1 | 21452.93 | 14177.20 | 1.51× |
| sqlite / events400-s1-c1-p50-bg1 | 123.67 | 97.05 | 1.27× |
| sqlite / events400-s1-c4-p25-bg1 | 330.33 | 212.49 | 1.55× |
| sqlite / events400-s1-c4-p50-bg1 | 342.91 | 228.19 | 1.50× |
| sqlite / events400-s16-c1-p50-bg1 | 288.98 | 221.91 | 1.30× |
| sqlite / events400-s16-c4-p25-bg1 | 756.07 | 506.45 | 1.49× |
| sqlite / events400-s16-c4-p50-bg1 | 790.08 | 547.03 | 1.44× |
| sqlite / jumbo-c1 | 109.58 | 62.55 | 1.75× |
| sqlite / jumbo-master-c1 | 109.98 | 62.87 | 1.75× |
| sqlite / memory4000-c4-p50 | 32658.09 | 11217.13 | 2.91× |
| postgres / events400-s1-c1-p25-bg0 | 39.53 | 37.30 | 1.06× |
| postgres / events400-s1-c1-p25-bg1 | 42.79 | 42.48 | 1.01× |
| postgres / events400-s16-c1-p25-bg0 | 96.35 | 77.49 | 1.24× |
| postgres / events400-s16-c1-p25-bg1 | 102.21 | 87.57 | 1.17× |
| postgres / events4000-s1-c1-p25-bg0 | 743.76 | 744.29 | 1.00× |
| postgres / events4000-s1-c1-p25-bg1 | 770.54 | 771.06 | 1.00× |
| postgres / events4000-s16-c1-p25-bg0 | 1312.03 | 1097.86 | 1.20× |
| postgres / events4000-s16-c1-p25-bg1 | 1340.78 | 1141.53 | 1.17× |
| postgres / events400-s1-c1-p50-bg1 | 46.56 | 47.34 | 0.98× |
| postgres / events400-s1-c4-p25-bg1 | 78.98 | 75.98 | 1.04× |
| postgres / events400-s1-c4-p50-bg1 | 92.40 | 92.74 | 1.00× |
| postgres / events400-s16-c1-p50-bg1 | 117.32 | 93.25 | 1.26× |
| postgres / events400-s16-c4-p25-bg1 | 202.29 | 173.27 | 1.17× |
| postgres / events400-s16-c4-p50-bg1 | 251.01 | 199.45 | 1.26× |
| postgres / jumbo-c1 | 39.55 | 40.29 | 0.98× |
| postgres / jumbo-master-c1 | 39.40 | 38.80 | 1.02× |
| postgres / memory4000-c4-p50 | 1375.41 | 1395.00 | 0.99× |

SQLite SQL events phase on4000 events /1source /25page: 5437.44 → 1923.22 ms (2.83×). This uses the actual Store reader in source copies from the frozen before archive and final source; one warmup then3 walks. SQL events includes driver/Scan, excludes metadata, explicit JSON summary decode, authority, Pack and HTTP. See sql-only-manifest.json. No overlays in acceptance binaries.

34 paired cases verified identical item/reference/title digests, correct cardinality, zero provider calls,16KiB SDK ceiling and no RSS-guard failures. Background is the unchanged5ms UI-read/sync-write synthetic loop; it is not a production load estimate.

Final checks:711 named Go race tests;102 storage tests with integration enabled, including11 explicit PostgreSQL-backed tests; no skips. Vet/build passed. Projection probe passes with -count=2 and fails against pre-optimization SQL in an isolated negative control. Timed/all-day ordering, receipt/revision fences, post-Pack revocation, cancellation, oversized markers and covering indexes remain covered.

First-page SLO: all16 backend/case combinations passed; C1≤50ms, C4≤100ms. Candidate-only paired-process peak external RSS: SQLite 51.27 MiB; PostgreSQL 41.72 MiB. PostgreSQL server RSS was not measured in this comparison. Sampling is best effort and is not a hard executor memory bound.

SQLite ordinary matrix cases improve1.27–2.68×; the single-round C4 memory stress improves2.91×. PostgreSQL16-source ordinary cases improve1.17–1.26×; single-source results are approximately unchanged, with individual cases up to1.9% slower. No universal speedup or p95 full-walk claim is made.

The initial ./... run failed because standalone diagnostic main programs from the previous turn lacked build-ignore tags; runtime packages passed. Added tags and reran the entire checker successfully. Initial raw failures remain in checks-initial/; details in verification-notes.json.

Implementation: Terra high, one sequential storage workstream; actual diff and integrated results reviewed by the coordinator. Only two runtime storage source files changed. Final authority remains a fresh transaction after Pack; no connection is retained through Pack or HTTP output. Temporary PostgreSQL stopped after all checks.
