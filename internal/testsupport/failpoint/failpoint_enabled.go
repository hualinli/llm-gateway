//go:build failpoint

// This file is compiled only with `-tags failpoint`. See doc.go for the three
// gates that keep failpoints out of production.
package failpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// HoldEnvVar overrides how long the `hold` action blocks. It exists so a
// scenario can prove "the process is stopped here" without waiting the default
// out when it only wants to observe the window.
const HoldEnvVar = "GATEWAY_FAILPOINT_HOLD"

// ExitCode is the process exit status of the `exit` action. It is deliberately
// outside the range any normal failure uses, so a log-only observer can tell
// "the failpoint ended this process" from "the process crashed".
const ExitCode = 197

// ErrInjected is wrapped by the error an `error` action returns from Hit.
var ErrInjected = errors.New("failpoint injected error")

// Options configures Attach.
type Options struct {
	// Lookup reads the process environment. Nil means os.Getenv.
	Lookup func(string) string
	// Profile is the runtime profile ("prod", "demo", "test"). Arming under
	// "prod" is refused.
	Profile string
}

// action is one failpoint's injected fault.
type action string

const (
	actionHold  action = "hold"
	actionKill  action = "kill"
	actionExit  action = "exit"
	actionPanic action = "panic"
	actionError action = "error"
)

var (
	active atomic.Bool

	mu      sync.Mutex
	armed   map[string]action
	fired   map[string]bool
	holdFor = defaultHold
)

// defaultHold is deliberately longer than any single failure-matrix step: a
// held window must survive until the runner's SIGKILL lands, and a hold that
// times out on its own would turn a deterministic window into a race. The
// timeout is the safety net for a human who forgot to kill the process.
const defaultHold = 120 * time.Second

// Attach parses GATEWAY_FAILPOINTS and arms this process.
//
// It returns an error — which every caller turns into a fatal startup failure —
// when failpoints are requested under the production profile. That is gate 2 of
// the three described in doc.go; gate 1 is the build tag, and gate 3 is the
// production preflight in cmd/gateway and cmd/metering.
func Attach(opts Options) error {
	lookup := opts.Lookup
	if lookup == nil {
		lookup = os.Getenv
	}
	raw := strings.TrimSpace(lookup(EnvVar))
	if raw == "" {
		return nil
	}
	profile := strings.ToLower(strings.TrimSpace(opts.Profile))
	if profile == "prod" || profile == "production" {
		return fmt.Errorf("%s is set but the runtime profile is production: failpoints stop or kill a process at a money-path window and are only available in demo/test builds", EnvVar)
	}
	parsed, err := parseList(raw)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return nil
	}
	if rawHold := strings.TrimSpace(lookup(HoldEnvVar)); rawHold != "" {
		parsedHold, err := time.ParseDuration(rawHold)
		if err != nil || parsedHold <= 0 {
			return fmt.Errorf("%s=%q is not a positive duration", HoldEnvVar, rawHold)
		}
		holdFor = parsedHold
	}

	mu.Lock()
	armed = parsed
	fired = make(map[string]bool, len(parsed))
	mu.Unlock()
	active.Store(true)

	names := make([]string, 0, len(parsed))
	for name, act := range parsed {
		names = append(names, fmt.Sprintf("%s=%s", name, act))
	}
	log.Printf("WARNING: failpoints ARMED in this process (test-only): %s hold=%s; this build must never serve production traffic",
		strings.Join(names, ","), holdFor)
	return nil
}

// parseList reads "name=action,other" (commas or whitespace separate entries).
func parseList(raw string) (map[string]action, error) {
	out := map[string]action{}
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	}) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rawAction, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s: empty failpoint name in %q", EnvVar, entry)
		}
		act := actionHold
		if ok {
			act = action(strings.ToLower(strings.TrimSpace(rawAction)))
		}
		switch act {
		case actionHold, actionKill, actionExit, actionPanic, actionError:
		default:
			return nil, fmt.Errorf("%s: failpoint %q has unknown action %q (want hold, kill, exit, panic or error)",
				EnvVar, name, act)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s: failpoint %q is listed twice", EnvVar, name)
		}
		out[name] = act
	}
	return out, nil
}

// Enabled reports whether this process has any armed failpoint. It is false in
// every build without the `failpoint` tag.
func Enabled() bool { return active.Load() }

// Armed reports whether one failpoint is armed. The commit helpers use it to
// decide whether a window needs to synthesise an outcome, which is cheaper and
// more obvious than a Hit that is expected to return an error.
func Armed(name string) bool {
	if !active.Load() {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	_, ok := armed[name]
	return ok
}

// Hit reports the process reaching a named window and applies that window's
// action. A nil return means "continue normally". Each name fires at most once
// per process.
func Hit(name string) error {
	if !active.Load() {
		return nil
	}
	return hit(name)
}

func hit(name string) error {
	mu.Lock()
	act, ok := armed[name]
	if !ok || fired[name] {
		mu.Unlock()
		return nil
	}
	fired[name] = true
	hold := holdFor
	mu.Unlock()

	emit("hit", name, act)
	switch act {
	case actionError:
		return fmt.Errorf("%w: %s", ErrInjected, name)
	case actionExit:
		os.Exit(ExitCode)
	case actionPanic:
		panic("failpoint " + name)
	case actionKill:
		// SIGKILL is not catchable and not blockable: the process ends here,
		// with whatever transaction state the window had.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable; keeps the compiler from treating this as a return
	default: // actionHold
		time.Sleep(hold)
		emit("released", name, act)
	}
	return nil
}

// emit writes one machine-readable marker line. scripts/failure-matrix.py tails
// the process log for `FAILPOINT {` and parses the JSON that follows.
func emit(event, name string, act action) {
	payload, err := json.Marshal(map[string]any{
		"event":  event,
		"name":   name,
		"action": string(act),
		"pid":    os.Getpid(),
		"at":     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		log.Printf("FAILPOINT {\"event\":%q,\"name\":%q,\"action\":%q,\"error\":%q}", event, name, act, err.Error())
		return
	}
	log.Printf("FAILPOINT %s", payload)
}
