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

// TestEventAttributionStaysForwardAndBackwardCompatible pins the ADR-0003 §2.8
// wire contract: events written before migration 000007 decode to "unknown"
// attribution, new events carry org/user/key/execution, and an unknown owner
// stays absent (SQL NULL) instead of becoming user 0.
func TestEventAttributionStaysForwardAndBackwardCompatible(t *testing.T) {
	legacy := `{"event_id":"evt-legacy","tenant_id":"tenant-a","model":"m","provider":"p","prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"cost_cents":3,"created_at":"2026-09-11T01:02:03Z"}`
	var old Event
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatal(err)
	}
	if old.OrgID != 0 || old.UserID != 0 || old.KeyID != "" || old.ExecutionID != "" {
		t.Fatalf("legacy event gained attribution: %+v", old)
	}
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"org_id"`, `"user_id"`, `"key_id"`, `"execution_id"`} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("unknown attribution was serialized as %s: %s", key, encoded)
		}
	}

	attributed := Event{
		EventID: "exec_abc", TenantID: "org:7", OrgID: 7, UserID: 42,
		KeyID: "key-1", ExecutionID: "exec_abc", Model: "m", Provider: "p",
		TotalTokens: 3, CostCents: 3, CreatedAt: old.CreatedAt,
	}
	encoded, err = json.Marshal(attributed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"org_id":7`, `"user_id":42`, `"key_id":"key-1"`, `"execution_id":"exec_abc"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("attribution %s missing from %s", want, encoded)
		}
	}
	var decoded Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OrgID != 7 || decoded.UserID != 42 || decoded.KeyID != "key-1" || decoded.ExecutionID != "exec_abc" {
		t.Fatalf("attribution lost in round trip: %+v", decoded)
	}

	// Legacy keys without an owner keep user_id NULL: the zero value must stay
	// distinguishable from a real user.
	ownerless := Event{EventID: "exec_ownerless", TenantID: "tenant-a", OrgID: 7, KeyID: "key-legacy"}
	encoded, err = json.Marshal(ownerless)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"user_id"`) {
		t.Fatalf("ownerless event serialized a user_id: %s", encoded)
	}
}
