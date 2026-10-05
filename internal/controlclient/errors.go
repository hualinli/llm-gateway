package controlclient

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sentinel errors returned by every Client method. They are the only error
// values the Console maps onto HTTP responses, so a new gRPC code cannot leak a
// raw status to a browser.
var (
	// ErrConflict means the operation_id was already used with a different
	// payload (gRPC ALREADY_EXISTS). Control never silently overwrites it.
	ErrConflict = errors.New("control: operation id already exists with a different payload")
	// ErrPreconditionFailed means expected_version did not match the current
	// snapshot version. Use CurrentVersion to read the version to retry with.
	ErrPreconditionFailed = errors.New("control: version precondition failed")
	// ErrUpstreamAuth means the Console could not authenticate to Control
	// (gRPC UNAUTHENTICATED or PERMISSION_DENIED). Serving HTTP maps this to
	// 502: the browser is not the party that must fix its credentials.
	ErrUpstreamAuth = errors.New("control: console is not authorized for the admin service")
	// ErrNotFound means the referenced key, organization or operation does not
	// exist (or is outside the asserted org scope).
	ErrNotFound = errors.New("control: not found")
	// ErrInvalidArgument means Control rejected the request as malformed.
	ErrInvalidArgument = errors.New("control: invalid admin request")
	// ErrUnavailable means Control could not be reached, the deadline expired,
	// or the server failed internally. The operation outcome is unknown for a
	// mutation; the deterministic operation_id makes a retry safe.
	ErrUnavailable = errors.New("control: admin service unavailable")
)

// PreconditionError carries the current version Control reported together with
// FAILED_PRECONDITION. Unwrap returns ErrPreconditionFailed so errors.Is keeps
// working.
type PreconditionError struct {
	// CurrentVersion is the snapshot version to retry with; nil when Control
	// did not report one.
	CurrentVersion *int64
	// Message is the bounded server message.
	Message string
}

func (e *PreconditionError) Error() string {
	if e == nil {
		return ErrPreconditionFailed.Error()
	}
	if e.CurrentVersion != nil {
		return fmt.Sprintf("%s: %s (current_version=%d)", ErrPreconditionFailed, e.Message, *e.CurrentVersion)
	}
	return fmt.Sprintf("%s: %s", ErrPreconditionFailed, e.Message)
}

func (e *PreconditionError) Unwrap() error { return ErrPreconditionFailed }

// CurrentVersion extracts the current snapshot version from a
// FAILED_PRECONDITION error. ok is false for every other error and for a
// precondition failure that carried no version.
func CurrentVersion(err error) (version int64, ok bool) {
	var precondition *PreconditionError
	if !errors.As(err, &precondition) || precondition.CurrentVersion == nil {
		return 0, false
	}
	return *precondition.CurrentVersion, true
}

// expectedVersionPattern matches the suffix internal/control appends to a
// FAILED_PRECONDITION message ("...; retry with expected_version=17").
var expectedVersionPattern = regexp.MustCompile(`expected_version=(-?[0-9]+)`)

// maxErrorMessageBytes bounds what a server message can add to a log line or an
// HTTP response body. Control messages are short; anything longer is a symptom,
// not information.
const maxErrorMessageBytes = 512

// mapError converts a gRPC status into the typed errors above. The original
// message is preserved but bounded; the code decides the class.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	// Context errors raised by the caller (cancel/deadline) are not control
	// failures and must stay recognizable.
	if errors.Is(err, context.Canceled) && status.Code(err) == codes.Canceled {
		return err
	}
	code := status.Code(err)
	message := boundedMessage(status.Convert(err).Message())
	switch code {
	case codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrConflict, message)
	case codes.FailedPrecondition:
		precondition := &PreconditionError{Message: message}
		if match := expectedVersionPattern.FindStringSubmatch(message); match != nil {
			if value, parseErr := strconv.ParseInt(match[1], 10, 64); parseErr == nil && value >= 0 {
				precondition.CurrentVersion = &value
			}
		}
		return precondition
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: %s", ErrUpstreamAuth, message)
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	case codes.InvalidArgument, codes.OutOfRange:
		return fmt.Errorf("%w: %s", ErrInvalidArgument, message)
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown, codes.Aborted, codes.ResourceExhausted:
		return fmt.Errorf("%w: %s", ErrUnavailable, message)
	case codes.Canceled:
		return fmt.Errorf("%w: %s", ErrUnavailable, message)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, message)
	}
}

func boundedMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "no server message"
	}
	if len(message) <= maxErrorMessageBytes {
		return message
	}
	return message[:maxErrorMessageBytes] + "..."
}

// retryableCode reports whether a failed attempt may be repeated. Only
// transport-level failures qualify: the request either never reached Control or
// its answer was lost, and every Client method carries an operation_id whose
// replay Control answers deterministically (ADR-0003 §2.2). Business rejections
// (ALREADY_EXISTS, FAILED_PRECONDITION, INVALID_ARGUMENT, NOT_FOUND) are final.
func retryableCode(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}
