# Calendar runtime pagination — bounded implementation review

2026-11-17. Reviewer: `gpt-6-astra`, high. Review is relative to the exact dirty-checkout baseline, not HEAD alone. The original plan review and E0–E3 artifacts remain unchanged.

## Source findings and corrections

- **P1 — request binding before refresh.** The original observer captured a ciphertext digest before OAuth transport could refresh. Request-local token plus binding are now captured atomically under the existing TokenSource mutex, after successful persistence and before calendar HTTP I/O. An OAuth error retains the attempted original snapshot. Actual refreshed-token first HTTP200/401 and overlapping same-provider refresh/stale-denial regressions pass. Rereading a current binding after response is not used to relabel older I/O.
- **P1 — explicit insufficient refresh scope.** Explicitly insufficient Google scope evidence closes only the still-current original authorization before persisting the downgraded token. Missing scope remains an ordinary refresh under the accepted same-grant assumption; broader legacy/full-calendar scopes are supported. Unknown historical scope additions cannot be inferred as changes, and scopes never open a detail receipt.
- **P2 — SQLite time semantics.** Actual shared cache writes now normalize scalar timestamps to UTC while retaining original payload offsets/timezones. Direct page API bounds normalize to UTC. Actual nonzero-offset, fractional-second, equal-instant and strict-overlap tests pass. Begin purges Google projection before opening receipts; no backfill was introduced.
- **P2 — mixed timed/date rows and covering eligibility.** Timed precedence is enforced before LIMIT. SQLite timed/date UNION arms retain their existing covering indexes; a pre-LIMIT outer guard rejects out-of-range timed rows admitted by stray dates. Pinned-driver EQP/VDBE tests check both arms without base-table cursor reads. No new index or pool setting was added.
- **P2 — mutation-only repair403.** Known write-only repair denial remains object-local, retains quarantine, and does not park a readable source. Unknown/mixed permission reasons remain conservative scoped closures.
- **P2 — obsolete repair auth failure.** An old401/invalid_grant after another refresh exits without parking the current read scope. Exact current-token denials still close the proper scope.

Astra inspected the corrections and reported no remaining actionable source findings in this bounded pass. This is not an exhaustive audit or deployment approval.

## Verified evidence so far

The final source check bundle is [calendar-runtime-r6-20261117-final](calendar-runtime-r6-20261117-final/). The complete race suite passed 706 named tests with no skips, the storage suite with integration enabled passed 97 named cases (11 explicit PostgreSQL-backed cases) with no skips, and vet/build passed. Source manifest did not change during these checks. Selected frontend source/status regressions passed 26 tests.

Actual SDK tests cover private caller context through sessions, credential-separated cursors, HTTP reauthorization, strict arguments/body limit, registration prerequisites, Store release before slow output/cancellation, and a post-Pack→actual Begin→fresh validation barrier that returns a bounded SDK error without the packed title. Separate SDK, JSON-RPC frame, HTTP body and raw HTTP/1 transport counters are present in the runtime harness.

## Final measurement verdict

**Astra: ACCEPTED for the approved isolated local R0–R4 implementation, with documented measurement limits.** No outstanding P1/P2 source findings remain from the bounded review. The reviewer checked all 159 source-manifest entries, harness/runner/binary identities, final checks and corrected evidence. This is not production, real-provider or deployment approval.

Final matrices: 24 speed cases and 28 completed candidate memory processes. Ordinary/wide first-page targets and SDK bounds pass. Candidate peak sampled RSS 39.23 MiB and self max 39.44 MiB. Four baseline dense4000/C4 processes failed the best-effort 256 MiB RSS guard; sampled overshoot 313.56–446.06 MiB is preserved as failure. Some SQLite first-page comparisons regress, and dense full traversal is substantially slower. E3 is historical supporting evidence, not the runtime numerical baseline.

Initial raw attempts are retained separately and excluded from final evidence: disposable-path rejection, an invalid 16-source seed, then an intermediate non-covering date-arm query. The final harness asserts distinct source claims, exact seeded count and complete traversal count.

Final speed protocol: 20 alternating paired warm first-page rounds, plus three separately reported paired full walks; full-walk results are medians/ranges, not 20-round p95 claims. Memory uses three complete walks per worker in fresh baseline/candidate processes with natural GC, a best-effort 256 MiB client-process RSS guard, and separate PostgreSQL cluster-child RSS sampling. No real credentials, providers, application database, production, Gateway, clients or Git publication are involved.
