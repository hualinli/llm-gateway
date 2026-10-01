//go:build !failpoint

package failpoint

import "errors"

// Options configures Attach. It mirrors the options of the tagged build so the
// call sites in cmd/gateway and cmd/metering compile identically either way.
type Options struct {
	// Lookup reads the process environment. Nil means os.Getenv.
	Lookup func(string) string
	// Profile is the runtime profile ("prod", "demo", "test"). The tagged build
	// refuses to arm failpoints under "prod".
	Profile string
}

// ErrInjected is the sentinel wrapped by an `error` action. In a build without
// the `failpoint` tag it is never returned by Hit; it exists so callers can
// reference it unconditionally.
var ErrInjected = errors.New("failpoint injected error")

// Attach is a no-op: this build has no failpoint implementation at all, so a
// failpoint environment cannot arm anything here.
func Attach(Options) error { return nil }

// Enabled reports whether this build can arm failpoints. It is always false
// without the `failpoint` build tag.
func Enabled() bool { return false }

// Armed reports whether one failpoint is armed. It is always false without the
// `failpoint` build tag.
func Armed(string) bool { return false }

// Hit reports the process reaching a failpoint window. It is always a no-op
// without the `failpoint` build tag.
func Hit(string) error { return nil }
