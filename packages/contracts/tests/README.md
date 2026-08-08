# Contract conformance vectors

`fixtures/` contains schema-independent boundary vectors. The package validates
their structure and coverage now; service owners must bind them to real
authenticated transport handlers when those handlers exist.

`doc-coverage/` contains focused Proto parser fixtures. The verification gate
builds them into Buf descriptors to prove line, block, detached, and trailing
comments are associated correctly and unrelated comments cannot mask an
undocumented top-level message or enum.
