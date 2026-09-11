package metering

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventIdentityWireMigration(t *testing.T) {
	for _, raw := range []string{
		`{"event_id":"evt_1","client_request_id":"client"}`,
		`{"request_id":"evt_1","client_request_id":"client"}`,
		`{"event_id":"evt_1","request_id":"evt_1","client_request_id":"client"}`,
	} {
		var event Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if event.EventID != "evt_1" || event.ClientRequestID != "client" {
			t.Fatalf("identity lost: %+v", event)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"request_id":`) || !strings.Contains(string(encoded), `"event_id":"evt_1"`) {
			t.Fatalf("unexpected wire format: %s", encoded)
		}
	}
	for _, raw := range []string{`{}`, `{"client_request_id":"client"}`, `{"event_id":"new","request_id":"old"}`} {
		var event Event
		if err := json.Unmarshal([]byte(raw), &event); err == nil {
			t.Fatalf("accepted invalid identity: %s", raw)
		}
	}
}
