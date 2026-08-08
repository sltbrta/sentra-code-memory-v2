// Package queryapi implements the four frozen Stage 04 QueryService RPCs —
// Ask, ListSources, GetHistory, and GetStatus — behind the authenticated
// owner-only gateway boundary.
//
// Every handler executes the generated Protovalidate rules on the decoded
// message first, then cross-checks the untrusted body identity against the
// authenticated peer — both strictly before any port invocation — and
// evaluates current authorization before admission or hydration. Unknown,
// unauthorized, and revoked reads share the one static not_found_or_denied
// outcome; identity mismatches return a Go error the transport maps to its
// static request-denied shape. Responses are constructed fresh per request
// and revalidated against the generated descriptors before return.
//
// Ask threads identity, authorization, conversation admission, engine answer,
// and exactly-once completion: a cancelled transport context commits no
// assistant turn, an engine failure commits a visibly failed completion, and
// an exact idempotent replay resolves the admitted key to its original
// outcome. The handler persists and retrieves nothing itself — engine,
// conversation, source-catalog, and authorization behavior all live behind
// the injected ports the runtime command composes.
package queryapi
