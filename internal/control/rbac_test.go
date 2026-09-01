package control

import "testing"

func TestPrincipalAuthorizedTenant(t *testing.T) {
	tests := []struct {
		name      string
		principal Principal
		requested string
		all       bool
		want      string
		wantErr   error
	}{
		{name: "admin selected tenant", principal: Principal{Subject: "admin", Role: RoleAdmin}, requested: "tenant-a", want: "tenant-a"},
		{name: "admin list all", principal: Principal{Subject: "admin", Role: RoleAdmin}, all: true},
		{name: "tenant user default", principal: Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}, want: "tenant-a"},
		{name: "tenant user own tenant", principal: Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}, requested: "tenant-a", want: "tenant-a"},
		{name: "tenant user cross tenant", principal: Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}, requested: "tenant-b", wantErr: ErrTenantForbidden},
		{name: "unknown role", principal: Principal{Subject: "user-a", Role: "operator"}, requested: "tenant-a", wantErr: ErrInvalidPrincipal},
		{name: "missing subject", principal: Principal{Role: RoleAdmin}, requested: "tenant-a", wantErr: ErrInvalidPrincipal},
		{name: "admin create requires tenant", principal: Principal{Subject: "admin", Role: RoleAdmin}, wantErr: errorString("tenant_id is required")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.principal.AuthorizedTenant(tt.requested, tt.all)
			if tt.wantErr != nil {
				if err == nil || err.Error() != tt.wantErr.Error() {
					t.Fatalf("error=%v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("tenant=%q, want %q", got, tt.want)
			}
		})
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
