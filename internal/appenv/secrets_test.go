package appenv

import "testing"

func TestIsDemoSecretMatchesShippedPlaceholdersOnly(t *testing.T) {
	for _, shipped := range []string{"demo-control-token", "demo-policy-read-token", " demo-control-token "} {
		if !IsDemoSecret(shipped) {
			t.Errorf("%q is a shipped placeholder and must be refused", shipped)
		}
	}
	for _, real := range []string{"0123456789abcdef", "demo-control-token-rotated", ""} {
		if IsDemoSecret(real) {
			t.Errorf("%q is not a shipped placeholder", real)
		}
	}
}
