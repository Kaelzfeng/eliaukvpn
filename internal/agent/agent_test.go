package agent

import (
	"testing"

	"eliaukvpn/internal/crypto"
)

func newTestIdentity(t *testing.T) *crypto.Identity {
	t.Helper()
	id, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
