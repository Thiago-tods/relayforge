package tenancy

import "testing"

// TestDestinationSubscriptionAndDeliveryPermissionMatrix pins the Phase 4.3 policy
// (ADR 27). Any change to it must be deliberate.
func TestDestinationSubscriptionAndDeliveryPermissionMatrix(t *testing.T) {
	type row struct {
		perm                      Permission
		owner, admin, dev, viewer bool
	}
	matrix := []row{
		{PermDestinationsRead, true, true, true, true},
		{PermDestinationsCreate, true, true, true, false},
		{PermDestinationsUpdate, true, true, true, false},
		{PermDestinationsPause, true, true, true, false},
		{PermDestinationsRotate, true, true, false, false},
		{PermDestinationsRevoke, true, true, false, false},
		{PermSubscriptionsRead, true, true, true, true},
		{PermSubscriptionsWrite, true, true, true, false},
		{PermDeliveriesRead, true, true, true, true},
		// Redrive (ADR 34): OWNER, ADMIN and DEVELOPER; VIEWER stays read-only.
		{PermDeliveriesRedrive, true, true, true, false},
	}
	for _, r := range matrix {
		for role, want := range map[Role]bool{RoleOwner: r.owner, RoleAdmin: r.admin, RoleDeveloper: r.dev, RoleViewer: r.viewer} {
			if got := Can(role, r.perm); got != want {
				t.Errorf("Can(%s, %s) = %v, want %v", role, r.perm, got, want)
			}
		}
	}
	if Can(Role("INTRUDER"), PermDestinationsRead) || Can(Role(""), PermDeliveriesRead) {
		t.Error("unknown roles must hold no permission")
	}
}
