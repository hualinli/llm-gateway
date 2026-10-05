package control

import (
	"testing"

	pb "llm-gateway/proto/control"
)

func orgPolicy(rpm int32, burst int32, models ...string) *pb.OrgPolicy {
	policy := &pb.OrgPolicy{RequestsPerMinute: &rpm, Burst: &burst}
	if models != nil {
		policy.ModelAllowlist = &pb.ModelAllowlist{Models: models}
	}
	return policy
}

// T09: publishing an org policy and then writing a credential or a route must
// not silently erase the policy. Before this, every legacy /config write and
// every key create/revoke dropped Config.org_policies, so the data-plane RPM and
// allowlist the Console had just published disappeared on the next key change.
func TestConfigPatchPreservesOrgPolicies(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	// Publish a policy the way the admin surface does.
	if _, err := srv.PatchConfig(ConfigPatch{
		LegacyKeys:  map[string]string{"sk-1": "tenant-a"},
		Routes:      RoutesToProto(map[string][]string{"m": {"p"}}),
		OrgPolicies: map[int64]*pb.OrgPolicy{7: orgPolicy(60, 10, "gpt-4o-mini")},
	}); err != nil {
		t.Fatal(err)
	}
	current := srv.CurrentConfig()
	if len(current.OrgPolicies) != 1 || current.OrgPolicies[7].GetRequestsPerMinute() != 60 {
		t.Fatalf("policy was not published: %+v", current.OrgPolicies)
	}
	if current.OrgPolicies[7].GetOrgId() != 7 {
		t.Fatalf("the map key was not stamped onto the policy: %+v", current.OrgPolicies[7])
	}

	// A credential-only write (the credential service path) preserves it.
	if _, err := srv.ApplyKeyRecords(current.KeyRecords, current.Routes); err != nil {
		t.Fatal(err)
	}
	if got := srv.CurrentConfig().OrgPolicies; len(got) != 1 || got[7].GetRequestsPerMinute() != 60 {
		t.Fatalf("a credential-only write dropped the org policy: %+v", got)
	}

	// A route-only patch preserves it too.
	if _, err := srv.PatchConfig(ConfigPatch{Routes: RoutesToProto(map[string][]string{"m": {"p", "q"}})}); err != nil {
		t.Fatal(err)
	}
	if got := srv.CurrentConfig().OrgPolicies; len(got) != 1 || got[7].GetBurst() != 10 {
		t.Fatalf("a route-only patch dropped the org policy: %+v", got)
	}

	// An explicit empty map clears it, and nil means "leave it alone".
	if _, err := srv.PatchConfig(ConfigPatch{OrgPolicies: map[int64]*pb.OrgPolicy{}}); err != nil {
		t.Fatal(err)
	}
	if got := srv.CurrentConfig().OrgPolicies; len(got) != 0 {
		t.Fatalf("an explicit empty policy map did not clear the policies: %+v", got)
	}
}

// Invalid policies are caller errors, not values to coerce: a zero RPM would
// otherwise mean "unlimited" once it reached the limiter.
func TestConfigPatchRejectsInvalidOrgPolicies(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[int64]*pb.OrgPolicy{
		"zero rpm":     {1: {RequestsPerMinute: func() *int32 { v := int32(0); return &v }()}},
		"zero burst":   {1: {Burst: func() *int32 { v := int32(0); return &v }()}},
		"nil policy":   {1: nil},
		"bad org id":   {0: orgPolicy(60, 10)},
		"mismatched":   {1: {OrgId: 2, RequestsPerMinute: func() *int32 { v := int32(60); return &v }()}},
		"empty model":  {1: orgPolicy(60, 10, "")},
		"model newlin": {1: orgPolicy(60, 10, "a\nb")},
	}
	for name, policies := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := srv.PatchConfig(ConfigPatch{OrgPolicies: policies}); err == nil {
				t.Fatalf("policy %+v was accepted", policies)
			}
		})
	}
}
