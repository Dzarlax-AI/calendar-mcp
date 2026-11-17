# Superseded intermediate measurement attempt

Raw files are retained. They measured the corrected 400-event / 16-source corpus, but are not acceptance evidence for the final source. Astra then identified that `start_at IS NULL` in the SQLite date UNION arm prevented covering index access. The predicate was moved to an outer pre-LIMIT guard, and pinned SQLite EQP/VDBE checks were added. The in-progress matrix was stopped rather than measuring a superseded query.

The final measurement protocol uses 20 alternating paired first-page rounds and three paired complete walks, reported separately. It does not claim 20 complete-walk samples. Memory remains three complete walks per worker in separate fresh processes. The final matrix is under `../calendar-runtime-r6-20261117-final/`.
