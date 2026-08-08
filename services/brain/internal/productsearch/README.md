# productsearch

Unified **Search / Ask** facade over profiles (backends, not products):

| Profile | Backend |
| --- | --- |
| local | hosted OpenLocal |
| hosted | path2 / product_neon from env |
| code | codecrawl |
| code_exact | codeindex P5 |
| auto | pick hosted → local → code |

Code and exact-code profiles share the repository ignore policy from
`.gitignore`, `.dockerignore`, and `.git/info/exclude`. This keeps generated,
secret, editor, dependency, and build outputs out of both indexing and results.
Lexical code ranking favors rare terms and files covering more of a multi-token
query.

CLI: `product-brain search` / `search-ask`.
