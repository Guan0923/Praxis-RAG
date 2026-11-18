package access

import (
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// TestBitmaskMergeDenyWins pins the documented merge formula: deny subtracts
// from the union of every applicable allow, so a whole-library deny overrides
// a file allow and a file deny restricts a whole-library allow, independently
// per operation.
func TestBitmaskMergeDenyWins(t *testing.T) {
	// Whole allow + file deny: the denied operation is refused, the rest stay.
	scope := NewScopeSet()
	scope.WholeAllow = 15
	scope.FileDeny["A.pdf"] = uint16(OpDelete)
	if !scope.CanReadDocument("A.pdf") || !scope.CanUpdateDocument("A.pdf") {
		t.Fatalf("whole allow + file deny(8) must keep read and update: %+v", scope)
	}
	if scope.CanDeleteDocument("A.pdf") {
		t.Fatal("file deny must restrict the whole-library delete")
	}
	if !scope.CanDeleteDocument("B.pdf") {
		t.Fatal("the file deny must not leak to other documents")
	}

	// Whole deny + file allow: the deny wins for that operation.
	scope = NewScopeSet()
	scope.WholeDeny = uint16(OpRead)
	scope.FileAllow["A.pdf"] = uint16(OpRead)
	if scope.CanReadDocument("A.pdf") {
		t.Fatal("whole-library deny must override the file allow")
	}

	// Operations combine independently.
	scope = NewScopeSet()
	scope.WholeAllow = uint16(OpRead | OpCreate)
	scope.FileAllow["a.md"] = uint16(OpUpdate)
	if !scope.CanCreate() || !scope.CanReadDocument("a.md") || !scope.CanUpdateDocument("a.md") {
		t.Fatalf("independent operation bits = %+v", scope)
	}
	if scope.CanDeleteDocument("a.md") {
		t.Fatal("delete was never allowed")
	}
}

// TestBitmaskAdminDefaultWithExplicitDeny covers acceptance scenario 1: the
// administrator keeps the default full allow, an explicit file deny removes
// exactly that operation for that file, and other files keep every
// operation.
func TestBitmaskAdminDefaultWithExplicitDeny(t *testing.T) {
	scope := NewScopeSet()
	scope.DefaultAllow = uint16(MaskAll)
	scope.FileDeny["A.pdf"] = uint16(OpDelete)

	if !scope.CanReadDocument("A.pdf") || !scope.CanUpdateDocument("A.pdf") {
		t.Fatal("A.pdf must stay readable and updatable")
	}
	if scope.CanDeleteDocument("A.pdf") {
		t.Fatal("A.pdf must not be deletable")
	}
	if !scope.CanDeleteDocument("B.pdf") || !scope.CanReadDocument("B.pdf") || !scope.CanUpdateDocument("B.pdf") {
		t.Fatal("B.pdf must keep every operation")
	}
}

// TestBitmaskRevokeAllowKeepsDeny covers acceptance scenario 2: revoking the
// allow of a rule leaves its deny in force.
func TestBitmaskRevokeAllowKeepsDeny(t *testing.T) {
	scope := NewScopeSet()
	scope.WholeAllow = uint16(OpRead)
	scope.FileDeny["A.pdf"] = uint16(OpRead)
	if scope.CanReadDocument("A.pdf") {
		t.Fatal("deny must already restrict the read")
	}
	// "Revoke allow" clears the allow side only; the deny stays.
	scope.WholeAllow = 0
	if scope.AllowsAnyRead() {
		t.Fatal("nothing is allowed any more")
	}
	if Operation(scope.EffectiveDocumentOrDefault("A.pdf"))&OpRead != 0 {
		t.Fatal("the file deny must still refuse the read")
	}
}

// TestBitmaskCreateVsUpdateSplit covers acceptance scenario 3: a create-only
// account may add new documents but not overwrite existing ones, and an
// update-only account may replace its granted file but not delete it.
func TestBitmaskCreateVsUpdateSplit(t *testing.T) {
	createOnly := NewScopeSet()
	createOnly.WholeAllow = uint16(OpCreate)
	if !createOnly.CanCreate() {
		t.Fatal("create-only must create")
	}
	if createOnly.CanUpdateDocument("existing.pdf") || createOnly.CanDeleteDocument("existing.pdf") {
		t.Fatal("create-only must not update or delete an existing document")
	}

	updateOnly := NewScopeSet()
	updateOnly.FileAllow["granted.md"] = uint16(OpUpdate)
	if !updateOnly.CanUpdateDocument("granted.md") {
		t.Fatal("update-only must replace its granted file")
	}
	if updateOnly.CanDeleteDocument("granted.md") || updateOnly.CanCreate() {
		t.Fatal("update-only must not delete or create")
	}
}

// TestBitmaskRestrictedRetrieval pins when retrieval must be treated as
// restricted: a whole-library read plus a file-level deny is still restricted.
func TestBitmaskRestrictedRetrieval(t *testing.T) {
	scope := NewScopeSet()
	scope.WholeAllow = uint16(OpRead)
	if scope.Restricted() {
		t.Fatal("an unrestricted whole-library read must not be restricted")
	}
	scope.FileDeny["secret.pdf"] = uint16(OpRead)
	if !scope.Restricted() {
		t.Fatal("whole-library read + file deny must be restricted")
	}
	if scope.CanReadDocument("secret.pdf") {
		t.Fatal("the denied file must not be readable")
	}
	if !scope.CanReadDocument("public.md") {
		t.Fatal("other files stay readable")
	}
}

// TestLegacyMaskMapping pins the historical permission conversion table.
func TestLegacyMaskMapping(t *testing.T) {
	if got := LegacyMask(state.PermissionRead, true); got != 1 {
		t.Fatalf("read -> %d, want 1", got)
	}
	if got := LegacyMask(state.PermissionWrite, true); got != 15 {
		t.Fatalf("whole write -> %d, want 15", got)
	}
	if got := LegacyMask(state.PermissionWrite, false); got != 13 {
		t.Fatalf("file write -> %d, want 13", got)
	}
	if got := LegacyPermission(1); got != state.PermissionRead {
		t.Fatalf("mask 1 projects to %q", got)
	}
	if got := LegacyPermission(13); got != state.PermissionWrite {
		t.Fatalf("mask 13 projects to %q", got)
	}
	if got := LegacyPermission(15); got != state.PermissionWrite {
		t.Fatalf("mask 15 projects to %q", got)
	}
	if got := LegacyPermission(4); got != "" {
		t.Fatalf("mask 4 (update only) projects to %q, want empty", got)
	}
	if got := LegacyPermission(0); got != "" {
		t.Fatalf("mask 0 projects to %q, want empty", got)
	}
}

// TestMaskValidation rejects unknown bits and out-of-range values.
func TestMaskValidation(t *testing.T) {
	if _, err := ParseMask(15); err != nil {
		t.Fatalf("15 must be valid: %v", err)
	}
	if _, err := ParseMask(0); err != nil {
		t.Fatalf("0 must be valid: %v", err)
	}
	for _, bad := range []int64{-1, 16, 100, 1 << 20} {
		if _, err := ParseMask(bad); err == nil {
			t.Fatalf("%d must be rejected", bad)
		}
	}
	if !MaskAllowsFileScope(0) || !MaskAllowsFileScope(uint16(OpRead | OpUpdate | OpDelete)) {
		t.Fatal("file-scope masks without create must be accepted")
	}
	if MaskAllowsFileScope(uint16(OpCreate)) || MaskAllowsFileScope(15) {
		t.Fatal("masks with the create bit must be rejected at file scope")
	}
}
