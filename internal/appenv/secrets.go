package appenv

import "strings"

// demoSecrets are the placeholder credentials that ship in docker-compose.yml
// and the demo tooling. Each one is long enough to pass a length check, so a
// production preflight must refuse them by value. Otherwise a deployment that
// starts from the compose file runs with a publicly known admin token.
var demoSecrets = map[string]struct{}{
	"demo-control-token":     {},
	"demo-policy-read-token": {},
}

// IsDemoSecret reports whether value is one of the placeholder credentials that
// ship with this repository.
func IsDemoSecret(value string) bool {
	_, ok := demoSecrets[strings.TrimSpace(value)]
	return ok
}
