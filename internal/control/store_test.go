package control

import "testing"

func TestRequireControlPostgres(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", " Yes "} {
		t.Setenv("CONTROL_REQUIRE_POSTGRES", value)
		if !requireControlPostgres() {
			t.Fatalf("CONTROL_REQUIRE_POSTGRES=%q was not recognized", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "random"} {
		t.Setenv("CONTROL_REQUIRE_POSTGRES", value)
		if requireControlPostgres() {
			t.Fatalf("CONTROL_REQUIRE_POSTGRES=%q was incorrectly recognized", value)
		}
	}
}
