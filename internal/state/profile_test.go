package state

import (
	"context"
	"testing"
	"time"
)

// profileISOTime renders a timestamp the same way the repositories store them.
func profileISOTime(moment time.Time) string {
	return moment.UTC().Format(isoTimestampFormat)
}

func TestCreateWithProfileAndByEmail(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	// The new columns and the unique index must exist on a freshly created
	// schema (ensureSchema creates them for new databases).
	for _, column := range []string{"email", "email_verified", "age", "gender"} {
		if !columnExists(t, stores, "users", column) {
			t.Fatalf("users is missing the %q column", column)
		}
	}
	indexed, err := stores.hasIndex(ctx, "users", "uq_users_email")
	if err != nil || !indexed {
		t.Fatalf("hasIndex(users, uq_users_email) = %v, %v", indexed, err)
	}

	alice, err := stores.Users.CreateWithProfile(ctx, "alice", "hash-a", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateWithProfile(alice) error = %v", err)
	}
	if alice.Email != "alice@example.com" || alice.EmailVerified || alice.Age != nil ||
		alice.Gender != "" || alice.AvatarUpdatedAt != nil {
		t.Fatalf("created profile = %+v", alice)
	}

	byEmail, err := stores.Users.ByEmail(ctx, "alice@example.com")
	if err != nil || byEmail == nil || byEmail.ID != alice.ID || byEmail.Email != "alice@example.com" {
		t.Fatalf("ByEmail(alice) = %+v, %v", byEmail, err)
	}
	if byEmail.EmailVerified {
		t.Fatalf("ByEmail() reported email_verified = true for a freshly created account")
	}
	if missing, err := stores.Users.ByEmail(ctx, "missing@example.com"); err != nil || missing != nil {
		t.Fatalf("ByEmail(missing) = %+v, %v", missing, err)
	}
	if empty, err := stores.Users.ByEmail(ctx, ""); err != nil || empty != nil {
		t.Fatalf("ByEmail(empty) = %+v, %v", empty, err)
	}

	// An account created without an address stores NULL, and several accounts
	// may do so because the unique index tolerates NULLs.
	noEmail, err := stores.Users.CreateWithProfile(ctx, "bob", "hash-b", "")
	if err != nil {
		t.Fatalf("CreateWithProfile(no email) error = %v", err)
	}
	if noEmail.Email != "" {
		t.Fatalf("no-email profile = %+v", noEmail)
	}
	noEmail2, err := stores.Users.Create(ctx, "carol", "hash-c")
	if err != nil {
		t.Fatalf("Create(carol) error = %v", err)
	}
	if noEmail2.Email != "" {
		t.Fatalf("Create(carol) profile = %+v", noEmail2)
	}

	// The e-mail unique index rejects a second account with the same address.
	if _, err := stores.Users.CreateWithProfile(ctx, "dave", "hash-d", "alice@example.com"); err == nil {
		t.Fatal("duplicate email was accepted")
	} else {
		mustBeError[*StateConflict](t, err)
	}
	// The username unique index still applies too.
	if _, err := stores.Users.CreateWithProfile(ctx, "alice", "hash-e", "other@example.com"); err == nil {
		t.Fatal("duplicate username was accepted")
	} else {
		mustBeError[*StateConflict](t, err)
	}
}

func TestUserUpdateProfile(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	alice, err := stores.Users.CreateWithProfile(ctx, "alice", "hash-a", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateWithProfile(alice) error = %v", err)
	}
	bob, err := stores.Users.Create(ctx, "bob", "hash-b")
	if err != nil {
		t.Fatalf("Create(bob) error = %v", err)
	}

	age := 30
	updated, err := stores.Users.UpdateProfile(ctx, alice.ID, "alice-renamed", &age, "female")
	if err != nil || updated == nil {
		t.Fatalf("UpdateProfile() = %+v, %v", updated, err)
	}
	if updated.Username != "alice-renamed" || updated.Age == nil || *updated.Age != 30 ||
		updated.Gender != "female" {
		t.Fatalf("updated profile = %+v", updated)
	}
	// The values are persisted, not only returned in memory.
	reloaded, err := stores.Users.ByID(ctx, alice.ID)
	if err != nil || reloaded == nil || reloaded.Username != "alice-renamed" ||
		reloaded.Age == nil || *reloaded.Age != 30 || reloaded.Gender != "female" {
		t.Fatalf("ByID after update = %+v, %v", reloaded, err)
	}

	// An empty username keeps the current one; a nil age and an empty gender
	// clear the optional fields.
	cleared, err := stores.Users.UpdateProfile(ctx, alice.ID, "", nil, "")
	if err != nil || cleared == nil {
		t.Fatalf("UpdateProfile(clear) = %+v, %v", cleared, err)
	}
	if cleared.Username != "alice-renamed" || cleared.Age != nil || cleared.Gender != "" {
		t.Fatalf("cleared profile = %+v", cleared)
	}

	// Renaming onto another account's username is a conflict.
	if _, err := stores.Users.UpdateProfile(ctx, bob.ID, "alice-renamed", nil, ""); err == nil {
		t.Fatal("duplicate username accepted by UpdateProfile")
	} else {
		mustBeError[*StateConflict](t, err)
	}
	// A missing account reports nil instead of an error.
	if missing, err := stores.Users.UpdateProfile(ctx, "no-such-id", "ghost", nil, ""); err != nil || missing != nil {
		t.Fatalf("UpdateProfile(missing) = %+v, %v", missing, err)
	}
}

func TestUserUpdateEmailAndPassword(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	alice, err := stores.Users.Create(ctx, "alice", "hash-a")
	if err != nil {
		t.Fatalf("Create(alice) error = %v", err)
	}
	bob, err := stores.Users.CreateWithProfile(ctx, "bob", "hash-b", "bob@example.com")
	if err != nil {
		t.Fatalf("CreateWithProfile(bob) error = %v", err)
	}

	ok, err := stores.Users.UpdateEmail(ctx, alice.ID, "alice@example.com", true)
	if err != nil || !ok {
		t.Fatalf("UpdateEmail() = %v, %v", ok, err)
	}
	reloaded, err := stores.Users.ByID(ctx, alice.ID)
	if err != nil || reloaded == nil || reloaded.Email != "alice@example.com" || !reloaded.EmailVerified {
		t.Fatalf("ByID after UpdateEmail = %+v, %v", reloaded, err)
	}
	// Re-setting the same address on the same account is allowed.
	if ok, err := stores.Users.UpdateEmail(ctx, bob.ID, "bob@example.com", true); err != nil || !ok {
		t.Fatalf("UpdateEmail(bob self) = %v, %v", ok, err)
	}
	// Taking another account's address is a conflict.
	if ok, err := stores.Users.UpdateEmail(ctx, alice.ID, "bob@example.com", false); err == nil {
		t.Fatalf("UpdateEmail(conflict) accepted: %v, %v", ok, err)
	} else {
		mustBeError[*StateConflict](t, err)
	}
	// A missing account reports false.
	if ok, err := stores.Users.UpdateEmail(ctx, "no-such-id", "x@example.com", false); err != nil || ok {
		t.Fatalf("UpdateEmail(missing) = %v, %v", ok, err)
	}

	// UpdatePassword replaces the stored hash seen by the login lookup.
	_, before, err := stores.Users.ByUsername(ctx, "alice")
	if err != nil || before != "hash-a" {
		t.Fatalf("ByUsername(before) = %q, %v", before, err)
	}
	if err := stores.Users.UpdatePassword(ctx, alice.ID, "hash-a-new"); err != nil {
		t.Fatalf("UpdatePassword() error = %v", err)
	}
	_, after, err := stores.Users.ByUsername(ctx, "alice")
	if err != nil || after != "hash-a-new" || after == before {
		t.Fatalf("ByUsername(after) = %q, %v", after, err)
	}
}

func TestUserAvatarRepository(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	alice, err := stores.Users.Create(ctx, "alice", "hash-a")
	if err != nil {
		t.Fatalf("Create(alice) error = %v", err)
	}

	if avatar, err := stores.Avatars.Get(ctx, alice.ID); err != nil || avatar != nil {
		t.Fatalf("Get(no avatar) = %+v, %v", avatar, err)
	}

	data := []byte("fake-png-bytes")
	if err := stores.Avatars.Set(ctx, alice.ID, "image/png", data); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	avatar, err := stores.Avatars.Get(ctx, alice.ID)
	if err != nil || avatar == nil {
		t.Fatalf("Get() = %+v, %v", avatar, err)
	}
	if avatar.UserID != alice.ID || avatar.ContentType != "image/png" ||
		avatar.Size != int64(len(data)) || avatar.SHA256 != sha256Sum(data) ||
		string(avatar.Content) != string(data) || avatar.UpdatedAt == "" {
		t.Fatalf("stored avatar = %+v", avatar)
	}

	// The users projection carries only the avatar timestamp.
	byID, err := stores.Users.ByID(ctx, alice.ID)
	if err != nil || byID == nil || byID.AvatarUpdatedAt == nil {
		t.Fatalf("ByID() avatar timestamp = %+v, %v", byID, err)
	}
	if *byID.AvatarUpdatedAt != avatar.UpdatedAt {
		t.Fatalf("AvatarUpdatedAt = %q, want %q", *byID.AvatarUpdatedAt, avatar.UpdatedAt)
	}

	// Set replaces the existing image.
	replacement := []byte("fake-png-bytes-v2")
	if err := stores.Avatars.Set(ctx, alice.ID, "image/jpeg", replacement); err != nil {
		t.Fatalf("Set(replacement) error = %v", err)
	}
	replaced, err := stores.Avatars.Get(ctx, alice.ID)
	if err != nil || replaced == nil || replaced.ContentType != "image/jpeg" ||
		string(replaced.Content) != string(replacement) {
		t.Fatalf("replaced avatar = %+v, %v", replaced, err)
	}

	// An image above the 2 MiB cap and an empty image are rejected.
	if err := stores.Avatars.Set(ctx, alice.ID, "image/png", make([]byte, MaxAvatarBytes+1)); err == nil {
		t.Fatal("oversized avatar was accepted")
	} else {
		mustBeError[*ValueError](t, err)
	}
	if err := stores.Avatars.Set(ctx, alice.ID, "image/png", nil); err == nil {
		t.Fatal("empty avatar was accepted")
	} else {
		mustBeError[*ValueError](t, err)
	}

	deleted, err := stores.Avatars.Delete(ctx, alice.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v", deleted, err)
	}
	if avatar, err := stores.Avatars.Get(ctx, alice.ID); err != nil || avatar != nil {
		t.Fatalf("Get(after delete) = %+v, %v", avatar, err)
	}
	byID, err = stores.Users.ByID(ctx, alice.ID)
	if err != nil || byID == nil || byID.AvatarUpdatedAt != nil {
		t.Fatalf("ByID(after delete) = %+v, %v", byID, err)
	}
	if deleted, err := stores.Avatars.Delete(ctx, alice.ID); err != nil || deleted {
		t.Fatalf("Delete(again) = %v, %v", deleted, err)
	}
}

func TestEmailVerificationRepository(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now().UTC()
	live := profileISOTime(now)
	expired := profileISOTime(now.Add(-time.Minute))
	future := profileISOTime(now.Add(10 * time.Minute))

	if err := stores.EmailCodes.Create(ctx, "a@example.com", "register", "digest-1", future); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	first, err := stores.EmailCodes.LatestActive(ctx, "a@example.com", "register", live)
	if err != nil || first == nil || first.CodeDigest != "digest-1" || first.Attempts != 0 || first.Consumed {
		t.Fatalf("LatestActive(first) = %+v, %v", first, err)
	}

	// Issuing a new code consumes the previous unused one in the same
	// transaction.
	if err := stores.EmailCodes.Create(ctx, "a@example.com", "register", "digest-2", future); err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	active, err := stores.EmailCodes.LatestActive(ctx, "a@example.com", "register", live)
	if err != nil || active == nil || active.CodeDigest != "digest-2" {
		t.Fatalf("LatestActive(second) = %+v, %v", active, err)
	}
	var consumed bool
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT consumed FROM email_verification_codes WHERE id = ?", first.ID).Scan(&consumed); err != nil {
		t.Fatalf("reading first code consumed flag: %v", err)
	}
	if !consumed {
		t.Fatal("previous code was not consumed by Create")
	}

	// Purposes and addresses are isolated from each other.
	if err := stores.EmailCodes.Create(ctx, "a@example.com", "email-change", "digest-3", future); err != nil {
		t.Fatalf("Create(other purpose) error = %v", err)
	}
	registerActive, err := stores.EmailCodes.LatestActive(ctx, "a@example.com", "register", live)
	if err != nil || registerActive == nil || registerActive.CodeDigest != "digest-2" {
		t.Fatalf("LatestActive(register) = %+v, %v", registerActive, err)
	}
	if other, err := stores.EmailCodes.LatestActive(ctx, "other@example.com", "register", live); err != nil || other != nil {
		t.Fatalf("LatestActive(other address) = %+v, %v", other, err)
	}

	// Attempts accumulate until the auth service rejects the code.
	if err := stores.EmailCodes.IncrementAttempts(ctx, active.ID); err != nil {
		t.Fatalf("IncrementAttempts() error = %v", err)
	}
	bumped, err := stores.EmailCodes.LatestActive(ctx, "a@example.com", "register", live)
	if err != nil || bumped == nil || bumped.Attempts != 1 {
		t.Fatalf("attempts = %+v, %v", bumped, err)
	}

	// A consumed code no longer counts as active.
	if err := stores.EmailCodes.Consume(ctx, active.ID); err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if leftover, err := stores.EmailCodes.LatestActive(ctx, "a@example.com", "register", live); err != nil || leftover != nil {
		t.Fatalf("LatestActive(after consume) = %+v, %v", leftover, err)
	}

	// Expired codes are invisible and can be pruned.
	if err := stores.EmailCodes.Create(ctx, "b@example.com", "register", "digest-4", expired); err != nil {
		t.Fatalf("Create(expired) error = %v", err)
	}
	if stale, err := stores.EmailCodes.LatestActive(ctx, "b@example.com", "register", live); err != nil || stale != nil {
		t.Fatalf("LatestActive(expired) = %+v, %v", stale, err)
	}
	removed, err := stores.EmailCodes.DeleteExpired(ctx, live)
	if err != nil || removed < 1 {
		t.Fatalf("DeleteExpired() = %d, %v", removed, err)
	}
	var remaining int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM email_verification_codes WHERE email = ?", "b@example.com").Scan(&remaining); err != nil {
		t.Fatalf("counting expired codes: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expired codes remaining = %d", remaining)
	}
}

func TestAppSettingsRepository(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	if value, found, err := stores.AppSettings.Get(ctx, "mail"); err != nil || found || value != "" {
		t.Fatalf("Get(missing) = %q, %v, %v", value, found, err)
	}
	if err := stores.AppSettings.Put(ctx, "mail", `{"enabled":true}`); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	value, found, err := stores.AppSettings.Get(ctx, "mail")
	if err != nil || !found || value != `{"enabled":true}` {
		t.Fatalf("Get() = %q, %v, %v", value, found, err)
	}
	// Put replaces the stored value for the same key.
	if err := stores.AppSettings.Put(ctx, "mail", `{"enabled":false}`); err != nil {
		t.Fatalf("Put(replace) error = %v", err)
	}
	if value, _, err := stores.AppSettings.Get(ctx, "mail"); err != nil || value != `{"enabled":false}` {
		t.Fatalf("Get(replaced) = %q, %v", value, err)
	}
	// Other keys stay independent.
	if err := stores.AppSettings.Put(ctx, "other", "value"); err != nil {
		t.Fatalf("Put(other) error = %v", err)
	}
	if value, found, err := stores.AppSettings.Get(ctx, "other"); err != nil || !found || value != "value" {
		t.Fatalf("Get(other) = %q, %v, %v", value, found, err)
	}
}

// TestUserProfileReadSurvivesLegacyAccount verifies that an account created by
// the legacy Create path reads back with zero-value profile fields instead of a
// scan error (all new columns are NULL or defaulted).
func TestUserProfileReadSurvivesLegacyAccount(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	legacy, err := stores.Users.Create(ctx, "legacy", "hash-l")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	byID, err := stores.Users.ByID(ctx, legacy.ID)
	if err != nil || byID == nil {
		t.Fatalf("ByID() = %+v, %v", byID, err)
	}
	if byID.Email != "" || byID.EmailVerified || byID.Age != nil || byID.Gender != "" {
		t.Fatalf("legacy profile = %+v", byID)
	}
	if _, err := stores.Users.ByEmail(ctx, "legacy@example.com"); err != nil {
		t.Fatalf("ByEmail(legacy) error = %v", err)
	}
}
