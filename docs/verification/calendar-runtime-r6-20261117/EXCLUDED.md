# Excluded initial measurement attempt

These raw files are retained and are not final runtime acceptance evidence.

- SQLite speed seeds initially failed the explicit disposable-path guard because system TMPDIR was outside the accepted tmp prefix. The runner now creates its own fixture directories under `/private/tmp`.
- The initial 16-source fixture repeatedly claimed the first source because the seed did not schedule that source beyond the fixed seed clock. Wide-case traversal contained 25 rather than the advertised 400 events. Those wide results are invalid.
- The old memory runner was stopped after the seed defect was identified. Completed raw runs, including a full-result baseline RSS guard termination, remain unchanged; interrupted runs have no completed measurement summary.
- The corrected harness asserts distinct source claims, exact inserted row count, and the expected full traversal count. The uniform corrected matrix lives in `../calendar-runtime-r6-20261117-v2/`.
- No runtime production source was changed for these harness corrections.
