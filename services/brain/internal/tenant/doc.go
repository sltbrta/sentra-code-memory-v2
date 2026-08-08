// Package tenant implements Phase 4 multi-tenant *local-file* MVP.
//
// # Surfaces
//
//   - Registry under a root: tenants.json + tenants/<id>/brains/<brainID>
//   - Create / Status / List / Disable / BrainDir
//   - AuthorizeBrainPath: fail-closed cross-tenant path check (TEN-005)
//
// # CLI
//
// product-brain tenant … and ask --tenant --tenant-root --brain-id.
// Passing --dir outside the tenant root yields cross_tenant_denied.
//
// # Not this package
//
// OpenFGA cloud admin, SCIM/GDPR full suite, multi-region HA (see
// docs/roadmap/DEFERRED-AND-NON-GOALS.md).
package tenant
