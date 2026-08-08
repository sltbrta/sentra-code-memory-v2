# VS Code CLI QA

This is a local smoke/quality report for the standalone CLI against a shallow
clone of `https://github.com/microsoft/vscode`, snapshot `c780ea96`. The clone
contained 17,598 tracked files and approximately 246 MB of tracked content.
The benchmark used the built `sentra-code-memory` binary on a local Apple
Silicon workstation; timings are directional, not portable SLO claims.

## Index timings

| Operation | Workers | Internal duration | Wall time |
| --- | ---: | ---: | ---: |
| cold index | 1 | 8,397 ms | 8.48 s |
| cold index | 4 | 4,845 ms | 4.90 s |
| cold index | 8 | 4,428 ms | 4.47 s |
| cold index | 16 | 4,984 ms | 5.07 s |
| warm refresh | 8 | 4,507 ms | 4.54 s |

The cold 8-worker run wrote `code-index.gob`. The warm run reported 13,016
unchanged files and 12,992 stamp-skipped files, with no gob rewrite. Eight
workers were fastest in this run; sixteen added contention rather than lowering
latency.

## Query timings and quality

Warm heuristic searches completed in roughly 100–182 ms. A natural-language
question was tested:

> Where is the extension host starter registered and how does
> `localProcessExtensionHost` start the extension host process?

The relevant-context response put
`src/vs/workbench/services/extensions/electron-browser/localProcessExtensionHost.ts`
first, followed by the extension-host process implementation. That is a useful
answer seed: it identifies the process-launching workbench seam and the source
file to inspect. The result also included unrelated agent-host documentation,
so the current heuristic answer is **useful but not citation-clean**; agents
should validate the top files rather than treat every ranked hit as an answer.

Exact symbol search was then used to verify the key contract:

```text
IExtensionHostStarter
→ src/vs/platform/extensions/common/extensionHostStarter.ts
```

Exact projection covered 12,544 P5 files in about 4.3 seconds and returned
source locations. Before the fix documented here, exact search failed on VS
Code with `bounded limit exceeded: snapshot results` (and then per-file token
limits). The standalone implementation now projects files individually under
hard caps, so one large repository does not fail as an all-or-nothing snapshot.

## Interpretation

- The CLI path works end-to-end on a real large repository.
- The warm heuristic lane is the fast conversational/context path.
- The exact lane is slower because it verifies every supported-language file,
  but it provides a stronger syntax-aware guarantee and source coordinates.
- Natural-language ranking still needs a second-stage semantic/reranking pass
  for citation-clean answers. That is an honest quality limitation, not hidden
  behind a success flag.
