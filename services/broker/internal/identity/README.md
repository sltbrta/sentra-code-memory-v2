# Local identity

`Mapper` accepts only trusted operating-system peer credentials and maps one
configured local UID to a principal and tenant. A body-supplied principal or
tenant or session is never authoritative; command and status cross-checks treat
them solely as exact consistency assertions against the peer session. All
missing and mismatched facts return the same denial.

Socket ownership, peer credential capture, and request decoding belong to the
gateway leaf.
