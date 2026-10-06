package control

import (
	"testing"
	"time"

	pb "llm-gateway/proto/control"
)

func TestModelAllowlistNullIsNotDenyAll(t *testing.T) {
	nullJSON, nullModels, err := canonicalModelAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	if nullJSON != "null" || nullModels != nil {
		t.Fatalf("nil allowlist = %q/%v, want null/nil (inherit)", nullJSON, nullModels)
	}
	emptyJSON, emptyModels, err := canonicalModelAllowlist(&pb.ModelAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if emptyJSON != "[]" || emptyModels == nil || len(emptyModels) != 0 {
		t.Fatalf("empty allowlist = %q/%v, want [] and a non-nil empty slice (deny-all)", emptyJSON, emptyModels)
	}
	if nullJSON == emptyJSON {
		t.Fatal("NULL and [] must stay distinguishable")
	}

	// The stored JSONB value keeps the same distinction.
	encodedNull, err := encodeModelAllowlist(nullModels)
	if err != nil {
		t.Fatal(err)
	}
	if encodedNull != nil {
		t.Fatalf("nil models must store SQL NULL, got %q", encodedNull)
	}
	encodedEmpty, err := encodeModelAllowlist(emptyModels)
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedEmpty) != "[]" {
		t.Fatalf("empty models must store [], got %q", encodedEmpty)
	}

	decodedNull, err := decodeModelAllowlist(nil)
	if err != nil || decodedNull != nil {
		t.Fatalf("NULL decoded to %v (err=%v), want nil (inherit)", decodedNull, err)
	}
	decodedEmpty, err := decodeModelAllowlist([]byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	if decodedEmpty == nil || decodedEmpty.GetModels() == nil || len(decodedEmpty.GetModels()) != 0 {
		t.Fatalf("[] decoded to %v, want a present empty allowlist", decodedEmpty)
	}
}

func TestModelAllowlistCanonicalizationIsDeterministic(t *testing.T) {
	sorted, models, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: []string{" gpt-b ", "gpt-a", "gpt-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if sorted != `["gpt-a","gpt-b"]` {
		t.Fatalf("canonical allowlist = %s", sorted)
	}
	if len(models) != 2 || models[0] != "gpt-a" || models[1] != "gpt-b" {
		t.Fatalf("canonical models = %v", models)
	}
	reordered, _, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: []string{"gpt-a", "gpt-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if reordered != sorted {
		t.Fatalf("model order changed the canonical form: %s vs %s", reordered, sorted)
	}
	if _, _, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: []string{"gpt-a", ""}}); err == nil {
		t.Fatal("empty model name was accepted")
	}
	if _, _, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: []string{"bad\nmodel"}}); err == nil {
		t.Fatal("control character in a model name was accepted")
	}
	tooMany := make([]string, maxAllowlistModels+1)
	for i := range tooMany {
		tooMany[i] = "m" + string(rune('a'+i%26))
	}
	if _, _, err := canonicalModelAllowlist(&pb.ModelAllowlist{Models: tooMany}); err == nil {
		t.Fatal("oversized allowlist was accepted")
	}
}

func TestCreateAPIKeyDigestIsBusinessFieldSensitive(t *testing.T) {
	owner := int64(7)
	other := int64(8)
	expiry := time.Date(2030, 5, 4, 3, 2, 1, 0, time.UTC)
	allowlist := `["gpt-a"]`

	base := createAPIKeyDigest(3, &owner, expiry, true, allowlist)
	if len(base) != 64 {
		t.Fatalf("digest = %q, want 64 hex characters", base)
	}
	if again := createAPIKeyDigest(3, &owner, expiry, true, allowlist); again != base {
		t.Fatal("digest is not deterministic")
	}
	bound := createAPIKeyDigest(3, &owner, expiry, true, allowlist, apiKeyDigestBinding{membershipID: 21, name: "production"})
	if bound == createAPIKeyDigest(3, &owner, expiry, true, allowlist, apiKeyDigestBinding{membershipID: 21, name: "staging"}) {
		t.Fatal("key name did not change the create digest")
	}
	if bound == createAPIKeyDigest(3, &owner, expiry, true, allowlist, apiKeyDigestBinding{membershipID: 22, name: "production"}) {
		t.Fatal("membership incarnation did not change the create digest")
	}
	// Sub-second precision is normalized away: the expiry effect is per second.
	nanos := createAPIKeyDigest(3, &owner, expiry.Add(500*time.Millisecond), true, allowlist)
	if nanos != base {
		t.Fatal("sub-second precision changed the digest")
	}
	// The generated key_id and raw secret are outputs, so they cannot be part
	// of the input digest; the request_id and actor fields live outside it.
	cases := map[string]string{
		"different org":       createAPIKeyDigest(4, &owner, expiry, true, allowlist),
		"different owner":     createAPIKeyDigest(3, &other, expiry, true, allowlist),
		"no owner":            createAPIKeyDigest(3, nil, expiry, true, allowlist),
		"owner zero":          createAPIKeyDigest(3, protoInt64(0), expiry, true, allowlist),
		"no expiry":           createAPIKeyDigest(3, &owner, expiry, false, allowlist),
		"different expiry":    createAPIKeyDigest(3, &owner, expiry.Add(time.Second), true, allowlist),
		"inherit allowlist":   createAPIKeyDigest(3, &owner, expiry, true, "null"),
		"deny-all allowlist":  createAPIKeyDigest(3, &owner, expiry, true, "[]"),
		"different allowlist": createAPIKeyDigest(3, &owner, expiry, true, `["gpt-b"]`),
	}
	for name, digest := range cases {
		if digest == base {
			t.Fatalf("%s produced the same digest as the base request", name)
		}
	}
}

func TestMutationDigestsDifferByKind(t *testing.T) {
	allowlist := "null"
	create := createAPIKeyDigest(1, nil, time.Time{}, false, allowlist)
	revoke := revokeAPIKeyDigest("key_1", 1)
	policy := applyOrgPolicyDigest(1, allowlist, nil, nil)
	seen := map[string]string{create: "create", revoke: "revoke", policy: "policy"}
	if len(seen) != 3 {
		t.Fatalf("digest kinds collided: %v", seen)
	}
	rpm := int32(60)
	if applyOrgPolicyDigest(1, allowlist, &rpm, nil) == policy {
		t.Fatal("requests_per_minute did not change the policy digest")
	}
	if applyOrgPolicyDigest(1, allowlist, nil, &rpm) == policy {
		t.Fatal("burst did not change the policy digest")
	}
	if revokeAPIKeyDigest("key_1", 0) == revoke {
		t.Fatal("org scope did not change the revoke digest")
	}
}

func protoInt64(value int64) *int64 { return &value }
