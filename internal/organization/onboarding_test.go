package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestActivationCodeIsOpaqueAndUsesIndependentCSPRNGValues(t *testing.T) {
	first, firstHash, err := generateActivationCode()
	if err != nil {
		t.Fatal(err)
	}
	second, secondHash, err := generateActivationCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, activationCodePrefix) || len(first) < len(activationCodePrefix)+40 {
		t.Fatalf("activation code shape = %q", first)
	}
	if first == second || firstHash == secondHash {
		t.Fatal("independent CSPRNG calls returned the same activation code")
	}
	digest := sha256.Sum256([]byte(first))
	if firstHash != hex.EncodeToString(digest[:]) || firstHash == first || len(firstHash) != 64 {
		t.Fatalf("activation code digest = %q", firstHash)
	}
}
