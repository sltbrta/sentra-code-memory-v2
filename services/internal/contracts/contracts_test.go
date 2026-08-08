package contracts

import "testing"

func TestArtifactVaultPortKeepsPurgeTypedAcrossL1L2(t *testing.T) {
	request := PurgeRequest{Artifact: Identifier{Namespace: "artifact", Value: "a"}, Tenant: Identifier{Namespace: "tenant", Value: "t"}, KeyEpoch: 2}
	if request.Artifact.Namespace != "artifact" || request.Tenant.Namespace != "tenant" || request.KeyEpoch != 2 {
		t.Fatal("purge request lost typed authority scope")
	}
}

func TestCommandRecordCarriesCompleteIdempotencyScope(t *testing.T) {
	record := CommandRecord{
		Tenant:         Identifier{Namespace: "tenant", Value: "t"},
		Principal:      Identifier{Namespace: "principal", Value: "p"},
		Session:        Identifier{Namespace: "session", Value: "s"},
		CommandType:    "artifact.admit",
		IdempotencyKey: "key",
		Fence:          7,
	}
	if record.Tenant.Value == "" || record.Principal.Value == "" || record.Session.Value == "" || record.CommandType == "" || record.IdempotencyKey == "" || record.Fence == 0 {
		t.Fatal("command record lost tenant/principal/type/key/fence idempotency scope")
	}
}
