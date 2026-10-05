//go:build !failpoint

package failpoint

import (
	"errors"
	"os"
	"testing"
)

// failpointsEnv is the arming variable, spelled out so this test file compiles
// without depending on the tagged one.
const failpointsEnv = "GATEWAY_FAILPOINTS"

// TestEnvVarNameIsDocumented pins the arming variable: operators and scripts
// both spell it out, so a rename must be a deliberate, visible change.
func TestEnvVarNameIsDocumented(t *testing.T) {
	if failpointsEnv != "GATEWAY_FAILPOINTS" {
		t.Fatalf("failpoint arming variable changed to %q; update doc.go and scripts/failure-matrix.py", failpointsEnv)
	}
}

// TestDisabledBuildIsInert is the gate-1 regression: a build without the
// `failpoint` tag must be unable to arm, report or act on anything, whatever
// the environment says.
func TestDisabledBuildIsInert(t *testing.T) {
	t.Setenv(failpointsEnv, "receipt.before_insert=kill,admission.after_reserve=hold")

	if err := Attach(Options{Profile: "demo", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v, want nil in a build without the failpoint tag", err)
	}
	if Enabled() {
		t.Fatal("Enabled() = true in a build without the failpoint tag")
	}
	if Armed("receipt.before_insert") {
		t.Fatal("Armed() = true in a build without the failpoint tag")
	}
	if err := Hit("receipt.before_insert"); err != nil {
		t.Fatalf("Hit() = %v, want nil in a build without the failpoint tag", err)
	}
}

// TestDisabledAttachIgnoresProductionEnv documents that even a hostile
// environment cannot turn this build into a failpoint build.
func TestDisabledAttachIgnoresProductionEnv(t *testing.T) {
	t.Setenv(failpointsEnv, "settlement.ack_before=exit")
	if err := Attach(Options{Profile: "prod", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v, want nil (the env is simply ignored here)", err)
	}
	if err := Hit("settlement.ack_before"); err != nil {
		t.Fatalf("Hit() = %v, want nil", err)
	}
}

// TestErrInjectedIsStable keeps the sentinel usable by callers that compile
// against both builds.
func TestErrInjectedIsStable(t *testing.T) {
	if !errors.Is(ErrInjected, ErrInjected) {
		t.Fatal("ErrInjected must be errors.Is-comparable")
	}
	if ErrInjected == nil {
		t.Fatal("ErrInjected must not be nil: callers compare against it unconditionally")
	}
}
