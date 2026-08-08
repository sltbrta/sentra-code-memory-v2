# Capability grants

This package implements exact, bounded attenuation for the local authority
plane. A child must retain the authenticated principal, tenant, fence, and
revocation epoch while narrowing actions, resources, path prefixes, resource
limits, and expiry. Wildcards and redelegation actions are rejected.

Use-time validation binds the exact nonce, fence, epoch, action, resource,
normalized path, and metered usage. Empty allowed paths permit only a use with
no path; a path-constrained grant requires one matching path. Empty limits
permit only empty usage, while a limited grant requires every dimension to be
reported within its bound. A constrained parent cannot attenuate either
dimension to empty.

Successful local validation is not a policy cache. The effect handler must
still perform a current authorization check before acting.
