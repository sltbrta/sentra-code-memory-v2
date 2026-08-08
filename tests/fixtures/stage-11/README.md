# Stage 11 fixtures

`multimodal/multimodal-cases.json` freezes four modality-family happy paths
and the acceptance-matrix negatives (oversized, malformed, media-type
mismatch, encrypted/unsupported, partial, duplicate, revoked, deleted).

These are contract descriptors only; they do not ship real media bytes or
parsers.
