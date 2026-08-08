// Package multimodalapi implements the five frozen MultimodalService methods
// behind injected ports on the authenticated local authority gateway. It holds
// no state between calls; the composed multimodal kernel owns durability and
// all multimodal authority. JPEG, compressed audio, video, and diarization
// remain deferred (DEF-010).
package multimodalapi
