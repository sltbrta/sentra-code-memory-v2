# productsearch

Unified **Search / Ask** facade over profiles (backends, not products):

| Profile | Backend |
| --- | --- |
| local | hosted OpenLocal |
| hosted | path2 / product_neon from env |
| code | codecrawl |
| code_exact | codeindex P5 |
| auto | pick hosted → local → code |

CLI: `product-brain search` / `search-ask`.
