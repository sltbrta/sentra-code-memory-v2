// Package github re-exports broker/internal/github for product composition
// outside services/broker/internal (Go internal-import rules).
package github

import (
	internal "github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/github"
)

type (
	API              = internal.API
	Broker           = internal.Broker
	Config           = internal.Config
	EffectGrant      = internal.EffectGrant
	FakeAPI          = internal.FakeAPI
	PRContent        = internal.PRContent
	PublicationTuple = internal.PublicationTuple
	PublishRequest   = internal.PublishRequest
)

const (
	ActionBranchPublish = internal.ActionBranchPublish
	ActionDraftPRCreate = internal.ActionDraftPRCreate
)

var (
	NewBroker    = internal.NewBroker
	NewFakeAPI   = internal.NewFakeAPI
	NewRESTAPI   = internal.NewRESTAPI
	ResolveToken = internal.ResolveToken
)
