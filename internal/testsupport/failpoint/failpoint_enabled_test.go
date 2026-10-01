//go:build failpoint

package failpoint

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// reset returns the package to "nothing armed". Tests in this file run
// sequentially and mutate package state, so each one starts from a clean slate.
func reset(t *testing.T) {
	t.Helper()
	mu.Lock()
	armed = nil
	fired = nil
	holdFor = defaultHold
	mu.Unlock()
	active.Store(false)
	t.Cleanup(func() {
		mu.Lock()
		armed = nil
		fired = nil
		holdFor = defaultHold
		mu.Unlock()
		active.Store(false)
	})
}

func TestAttachParsesEveryActionAndDefaultsToHold(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "a.b, c.d=kill ;e.f=exit,g.h=panic,i.j=error")
	t.Setenv(HoldEnvVar, "5ms")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v", err)
	}
	want := map[string]action{
		"a.b": actionHold, "c.d": actionKill, "e.f": actionExit,
		"g.h": actionPanic, "i.j": actionError,
	}
	mu.Lock()
	got := armed
	hold := holdFor
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("armed=%v, want %v", got, want)
	}
	for name, act := range want {
		if got[name] != act {
			t.Fatalf("armed[%s]=%q, want %q", name, got[name], act)
		}
	}
	if hold != 5*time.Millisecond {
		t.Fatalf("hold=%s, want 5ms", hold)
	}
	// The arming variable is spelled out in doc.go and in
	// scripts/failure-matrix.py; a rename must break this test loudly.
	if EnvVar != "GATEWAY_FAILPOINTS" {
		t.Fatalf("EnvVar=%q, want GATEWAY_FAILPOINTS", EnvVar)
	}
}

func TestAttachRefusesProductionProfile(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "receipt.before_insert=kill")
	err := Attach(Options{Profile: "prod", Lookup: os.Getenv})
	if err == nil {
		t.Fatal("Attach() = nil under the production profile with failpoints armed; want a refusal")
	}
	if !strings.Contains(err.Error(), EnvVar) {
		t.Fatalf("refusal %q does not name %s", err, EnvVar)
	}
	if Enabled() {
		t.Fatal("a refused Attach must leave the process disarmed")
	}
}

func TestAttachRefusesUnknownActionAndDuplicates(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "a.b=nuke")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err == nil {
		t.Fatal("Attach() accepted an unknown action")
	}
	reset(t)
	t.Setenv(EnvVar, "a.b=kill,a.b=hold")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err == nil {
		t.Fatal("Attach() accepted a duplicate failpoint")
	}
}

func TestAttachRefusesNonPositiveHold(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "a.b")
	t.Setenv(HoldEnvVar, "0s")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err == nil {
		t.Fatal("Attach() accepted a non-positive hold duration")
	}
}

func TestEmptyEnvArmsNothing(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "  ")
	if err := Attach(Options{Profile: "prod", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() with an empty env = %v, want nil even in production", err)
	}
	if Enabled() {
		t.Fatal("Enabled() = true with an empty failpoint env")
	}
}

func TestHitErrorActionWrapsErrInjectedAndFiresOnce(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "receipt.commit_response_lost=error")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v", err)
	}
	if !Armed("receipt.commit_response_lost") {
		t.Fatal("Armed() = false for an armed failpoint")
	}
	err := Hit("receipt.commit_response_lost")
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("Hit() = %v, want ErrInjected", err)
	}
	// Once per process: the second hit must not fire again, or a scenario
	// could not tell the injected window from a retry loop.
	if again := Hit("receipt.commit_response_lost"); again != nil {
		t.Fatalf("second Hit() = %v, want nil (failpoints fire at most once)", again)
	}
}

func TestHitUnarmedNameIsNil(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "a.b=kill")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v", err)
	}
	if err := Hit("c.d"); err != nil {
		t.Fatalf("Hit(unarmed) = %v, want nil", err)
	}
}

func TestHoldBlocksThenReleasesAndLogsBothMarkers(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "admission.after_reserve")
	t.Setenv(HoldEnvVar, "40ms")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v", err)
	}

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	started := time.Now()
	if err := Hit("admission.after_reserve"); err != nil {
		t.Fatalf("Hit() = %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("hold returned after %s, want >= 40ms", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, `"event":"hit"`) || !strings.Contains(out, `"name":"admission.after_reserve"`) {
		t.Fatalf("missing hit marker in %q", out)
	}
	if !strings.Contains(out, `"event":"released"`) {
		t.Fatalf("missing release marker in %q", out)
	}
}

func TestPanicActionPanics(t *testing.T) {
	reset(t)
	t.Setenv(EnvVar, "settlement.before_commit=panic")
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("Attach() = %v", err)
	}
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("panic action did not panic")
		}
		if !strings.Contains(fmt.Sprint(recovered), "settlement.before_commit") {
			t.Fatalf("panic value %v does not name the failpoint", recovered)
		}
	}()
	_ = Hit("settlement.before_commit")
}

// TestKillAndExitActionsRunInASubprocess proves the two terminal actions really
// end the process, which is the whole point of the kill windows: they are
// driven by re-executing this test binary as a child.
func TestKillAndExitActionsRunInASubprocess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		action   string
		wantCode int
	}{
		{name: "kill", action: "kill", wantCode: -1}, // killed by SIGKILL
		{name: "exit", action: "exit", wantCode: ExitCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestFailpointSubprocessHelper", "-test.v")
			cmd.Env = append(os.Environ(),
				"FAILPOINT_SUBPROCESS="+tc.action,
				EnvVar+"=receipt.after_insert_before_commit="+tc.action,
				"GATEWAY_PROFILE=test",
			)
			out, err := cmd.CombinedOutput()
			if tc.wantCode == -1 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("child exited with %v (output %q), want SIGKILL", err, out)
				}
				if status, ok := exitErr.Sys().(interface{ Signal() os.Signal }); ok && status.Signal() != nil {
					if got := status.Signal().String(); got != "killed" {
						t.Fatalf("child signal = %s, want killed; output=%q", got, out)
					}
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("child exited with %v (output %q), want exit code %d", err, out, tc.wantCode)
				}
				if exitErr.ExitCode() != tc.wantCode {
					t.Fatalf("child exit code = %d, want %d; output=%q", exitErr.ExitCode(), tc.wantCode, out)
				}
			}
			if !strings.Contains(string(out), `"event":"hit"`) {
				t.Fatalf("child did not log the hit marker before dying: %q", out)
			}
		})
	}
}

// TestFailpointSubprocessHelper is not a real test: it is the child body of
// TestKillAndExitActionsRunInASubprocess.
func TestFailpointSubprocessHelper(t *testing.T) {
	action := os.Getenv("FAILPOINT_SUBPROCESS")
	if action == "" {
		t.Skip("helper: only runs as a child process")
	}
	if err := Attach(Options{Profile: "test", Lookup: os.Getenv}); err != nil {
		t.Fatalf("child Attach() = %v", err)
	}
	_ = Hit("receipt.after_insert_before_commit")
	t.Fatalf("the %s action returned instead of ending the process", action)
}
