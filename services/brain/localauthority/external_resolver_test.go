package localauthority_test

import (
	"context"
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
)

type externalResolver struct{}

func (externalResolver) Current(context.Context, localauthority.Identifier) (localauthority.KeyMaterial, error) {
	return localauthority.KeyMaterial{
		Reference: localauthority.KeyReference{
			Root:  localauthority.Identifier{Namespace: "key-root", Value: "tenant"},
			KeyID: localauthority.Identifier{Namespace: "key", Value: "external"},
			Epoch: 1,
		},
		RootKey: make([]byte, localauthority.RootKeyBytes),
	}, nil
}

func (externalResolver) Resolve(context.Context, localauthority.Identifier, uint64) (localauthority.KeyMaterial, error) {
	return localauthority.KeyMaterial{}, errors.New("test resolver: epoch unavailable")
}

var _ localauthority.KeyResolver = externalResolver{}
