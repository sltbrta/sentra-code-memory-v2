# Stage 03 mixed-P5 fixture seeds

This directory tracks six small seeds: one each for Go, TypeScript, Python,
Rust, and Java plus one intentionally malformed TypeScript input. The contract
test expands them only inside a temporary Git repository and commits two real
trees; generated corpus bytes are never checked in.

`mixed-p5/delta-manifest.json` freezes 100 delta records: 25 adds, 25
modifications, 25 renames, and 25 deletes, balanced across P5. A rename counts
as one operation with an old and new repository-relative endpoint. Therefore
the manifest has exactly 100 operation records and 125 distinct path endpoints;
the test requires Git `-M100%` to report the same 100 records so rename/path
counting cannot drift.
