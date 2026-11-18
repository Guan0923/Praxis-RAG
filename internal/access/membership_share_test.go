package access

import (
	"context"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

type fixedEntitlements struct {
	can   bool
	err   error
	calls int
}

func (f *fixedEntitlements) CanShare(context.Context, string) (bool, error) {
	f.calls++
	return f.can, f.err
}

// TestCanShareAccountKeepsRoleContractAndAddsPaidPlans pins the compatibility
// rule: admin/vip keep sharing, an ordinary account shares only while its paid
// entitlement says so, and expiry never mutates the stored role.
func TestCanShareAccountKeepsRoleContractAndAddsPaidPlans(t *testing.T) {
	ordinary := state.User{ID: "u1", Role: state.RoleUser, Enabled: true}
	provider := &fixedEntitlements{can: true}
	service := &Service{entitlements: provider}
	if allowed, err := service.CanShareAccount(context.Background(), ordinary); err != nil || !allowed {
		t.Fatalf("paid ordinary account allowed=%v err=%v, want true/nil", allowed, err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}

	provider.can = false
	if allowed, _ := service.CanShareAccount(context.Background(), ordinary); allowed {
		t.Fatal("expired paid account must not share")
	}
	if ordinary.Role != state.RoleUser {
		t.Fatalf("role changed to %q; purchase/expiry must not mutate role", ordinary.Role)
	}

	for _, role := range []string{state.RoleAdmin, state.RoleVIP} {
		provider.calls = 0
		user := state.User{ID: "role-" + role, Role: role, Enabled: true}
		if allowed, err := service.CanShareAccount(context.Background(), user); err != nil || !allowed {
			t.Fatalf("%s allowed=%v err=%v, want true/nil", role, allowed, err)
		}
		if provider.calls != 0 {
			t.Fatalf("%s unexpectedly consulted membership %d times", role, provider.calls)
		}
	}

	disabled := state.User{ID: "off", Role: state.RoleVIP, Enabled: false}
	if allowed, _ := service.CanShareAccount(context.Background(), disabled); allowed {
		t.Fatal("disabled account must not share")
	}
}
