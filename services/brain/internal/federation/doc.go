// Package federation implements Phase 5 federated ask over *local* brains.
//
// # Flow (FED-002 first)
//
//  1. FilterCards — principal/tenant/region + AllowedFor (no open yet)
//  2. RankCards — topic overlap and cost hint
//  3. MintCapability — short-lived attenuated grant
//  4. OpenLocal + AnswerOpts per selected card
//  5. Merge labeled answers; never invent cites from denied brains
//
// # CLI
//
// product-brain federated-ask --q --principal --cards path:id[:allow+…]
//
// Multi-host mesh / HTTPS transport is out of scope (NG-FED-MESH).
package federation
