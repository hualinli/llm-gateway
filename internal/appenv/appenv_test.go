package appenv

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestFromEnvProfiles(t *testing.T) {
	cases := []struct {
		name    string
		gateway string
		appEnv  string
		want    Profile
		wantErr bool
	}{
		{name: "empty defaults to demo", want: Demo},
		{name: "gateway prod", gateway: "prod", want: Prod},
		{name: "gateway production", gateway: "production", want: Prod},
		{name: "gateway demo", gateway: "demo", want: Demo},
		{name: "gateway test", gateway: "test", want: Test},
		{name: "gateway case and spaces", gateway: "  PROD ", want: Prod},
		{name: "app env fallback", appEnv: "prod", want: Prod},
		{name: "app env production fallback", appEnv: "production", want: Prod},
		{name: "app env demo fallback", appEnv: "demo", want: Demo},
		{name: "gateway wins over app env", gateway: "test", appEnv: "prod", want: Test},
		{name: "blank gateway falls back to app env", gateway: "   ", appEnv: "prod", want: Prod},
		{name: "unknown gateway profile", gateway: "staging", wantErr: true},
		{name: "unknown app env profile", appEnv: "staging", wantErr: true},
		{name: "unknown is never a silent demo", gateway: "false", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GATEWAY_PROFILE", tc.gateway)
			t.Setenv("APP_ENV", tc.appEnv)
			got, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FromEnv() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("FromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProfileHelpers(t *testing.T) {
	if !Prod.IsProduction() || Demo.IsProduction() || Test.IsProduction() || Profile("").IsProduction() {
		t.Fatal("IsProduction must be true for prod only")
	}
	if Prod.String() != "prod" || Test.String() != "test" {
		t.Fatalf("String() = %q/%q", Prod.String(), Test.String())
	}
}

func TestValidateAggregatesEveryFailure(t *testing.T) {
	checks := []Check{
		{Name: "OK_ONE", Detail: "already satisfied", OK: true},
		{Name: "MISSING_ONE", Detail: "first requirement", OK: false},
		{Name: "MISSING_TWO", Detail: "second requirement", OK: false},
	}
	err := Validate(Prod, checks)
	if err == nil {
		t.Fatal("production must reject failing checks")
	}
	message := err.Error()
	for _, want := range []string{"MISSING_ONE: first requirement", "MISSING_TWO: second requirement"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "OK_ONE") {
		t.Fatalf("error %q reports a satisfied check", message)
	}
	lines := strings.Split(message, "\n")
	if len(lines) != 3 {
		t.Fatalf("error should aggregate both failures on separate lines, got %q", message)
	}

	if err := Validate(Prod, []Check{{Name: "OK", Detail: "ok", OK: true}}); err != nil {
		t.Fatalf("production with all checks passing returned %v", err)
	}
	for _, profile := range []Profile{Demo, Test, ""} {
		if err := Validate(profile, checks); err != nil {
			t.Fatalf("profile %q must not enforce production checks: %v", profile, err)
		}
	}
}

func TestLogMakesModeUnmistakable(t *testing.T) {
	prod := captureLog(t, func() { Log(Prod) })
	if !strings.Contains(prod, "runtime profile=prod (production checks enforced)") {
		t.Fatalf("prod log line = %q", prod)
	}
	for _, profile := range []Profile{Demo, Test} {
		line := captureLog(t, func() { Log(profile) })
		want := "runtime profile=" + string(profile) + " WARNING: demo mode, production checks are not enforced"
		if !strings.Contains(line, want) {
			t.Fatalf("log line for %q = %q, want %q", profile, line, want)
		}
	}
	empty := captureLog(t, func() { Log("") })
	if !strings.Contains(empty, "runtime profile=demo WARNING") {
		t.Fatalf("empty profile log line = %q", empty)
	}
}

func TestBoolEnv(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, " yes ": true,
		"0": false, "false": false, "no": false, "on": false, "": false, "junk": false,
	}
	for value, want := range cases {
		t.Setenv("APPENV_BOOL", value)
		if got := BoolEnv("APPENV_BOOL"); got != want {
			t.Fatalf("BoolEnv(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestStringEnv(t *testing.T) {
	t.Setenv("APPENV_STRING", "")
	if got := StringEnv("APPENV_STRING", ":8090"); got != ":8090" {
		t.Fatalf("unset StringEnv = %q", got)
	}
	t.Setenv("APPENV_STRING", "   ")
	if got := StringEnv("APPENV_STRING", ":8090"); got != ":8090" {
		t.Fatalf("blank StringEnv = %q", got)
	}
	t.Setenv("APPENV_STRING", "127.0.0.1:9090")
	if got := StringEnv("APPENV_STRING", ":8090"); got != "127.0.0.1:9090" {
		t.Fatalf("set StringEnv = %q", got)
	}
}

func TestPositiveIntEnvAndAtLeast(t *testing.T) {
	t.Setenv("APPENV_INT", "")
	if value, ok := PositiveIntEnv("APPENV_INT"); ok || value != 0 {
		t.Fatalf("unset PositiveIntEnv = (%d, %v)", value, ok)
	}
	for _, value := range []string{"0", "-1", "abc", "1.5"} {
		t.Setenv("APPENV_INT", value)
		if _, ok := PositiveIntEnv("APPENV_INT"); ok {
			t.Fatalf("PositiveIntEnv(%q) reported ok", value)
		}
	}
	t.Setenv("APPENV_INT", " 256 ")
	value, ok := PositiveIntEnv("APPENV_INT")
	if !ok || value != 256 {
		t.Fatalf("PositiveIntEnv = (%d, %v)", value, ok)
	}
	if !AtLeast("APPENV_INT", 16) || !AtLeast("APPENV_INT", 256) {
		t.Fatal("AtLeast rejected a large enough value")
	}
	if AtLeast("APPENV_INT", 257) {
		t.Fatal("AtLeast accepted a too small value")
	}
	t.Setenv("APPENV_INT", "8")
	if !AtLeast("APPENV_INT", 8) || AtLeast("APPENV_INT", 9) {
		t.Fatal("AtLeast boundary is wrong")
	}
}

func TestEnvLookupReadsOSEnv(t *testing.T) {
	t.Setenv("APPENV_LOOKUP", "value")
	if got := EnvLookup("APPENV_LOOKUP"); got != "value" {
		t.Fatalf("EnvLookup = %q", got)
	}
}

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	fn()
	return buf.String()
}
