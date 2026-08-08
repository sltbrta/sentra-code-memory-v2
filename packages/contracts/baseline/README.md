# Contract compatibility baseline

`contracts-v1.binpb` is a Buf file-descriptor image generated from `proto/`
with Buf `1.71.0`. It is the checked-in FILE-rule compatibility reference used
by `tools/verify.sh`; it is not a second hand-authored schema authority.

Its current SHA-256 is
`242915401b39e03e13360bb088b644d2727b084a81e2bfda7148340fd7596dc1`.
Replacement is an orchestrator-owned single-writer operation requiring a
tracked accepted compatibility decision and a separately reviewed explicit Buf
build command. Ordinary compatible changes must pass against this descriptor
without changing it.

The Stage 11 A0 replacement (on top of Stage 06 tracer) was built from the accepted additive multimodal contract with:

```sh
buf build --config buf.yaml proto --as-file-descriptor-set \
  --exclude-source-info -o baseline/contracts-v1.binpb
```
