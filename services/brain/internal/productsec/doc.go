// Package productsec implements Phase 2 residual product security.
//
// # Profiles
//
//   - single_user (default): offline brain; Authorize always allows.
//   - multi_principal: Owner + Grants; non-owners get ErrDenied without
//     disclosing whether the corpus exists.
//
// # Durability helpers
//
//   - security.json beside the brain dir (profile, owner, grants, evidence digest)
//   - SealSession / OpenSealedSession for encrypted turn frames under sessions/
//   - DigestFile / UpdateEvidenceDigest for gardener non-mutation checks
//
// # Integration
//
// hosted.Client loads ContextFromBrain on OpenLocal; AnswerOpts calls
// Authorize before Retrieve. Full chunk-byte ArtifactVault migration remains
// a documented partial (NG-VAULT-CHUNKS).
package productsec
