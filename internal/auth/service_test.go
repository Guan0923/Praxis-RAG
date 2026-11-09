package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

// newTestService opens the disposable MySQL schema and truncates it.
func newTestService(t *testing.T, registration bool, clock func() time.Time) (*Service, *state.Stores) {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      5,
		MaxOverflow:   10,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	options := Options{RegistrationEnabled: registration, SessionTTL: time.Hour}
	if clock != nil {
		options.Now = clock
	}
	return NewService(stores.Users, stores.Sessions, options), stores
}

func assertAuthError(t *testing.T, err error, kind ErrorKind, message string) {
	t.Helper()
	var authError *Error
	if !errors.As(err, &authError) {
		t.Fatalf("error = %v, want *auth.Error", err)
	}
	if authError.Kind != kind || authError.Message != message {
		t.Fatalf("error = %+v, want kind=%d message=%q", authError, kind, message)
	}
}

func TestRegisterFirstUserIsAdmin(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()

	first, err := service.Register(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Register(first) error = %v", err)
	}
	if !first.User.IsAdmin() || first.Token == "" || first.CSRF == "" || first.ExpiresAt.IsZero() {
		t.Fatalf("first login = %+v", first)
	}
	session, err := service.Authenticate(ctx, first.Token)
	if err != nil || session == nil || session.User.ID != first.User.ID || session.CSRF != first.CSRF {
		t.Fatalf("Authenticate() = %+v, %v", session, err)
	}

	second, err := service.Register(ctx, "bob", "password-two")
	if err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}
	if second.User.Role != state.RoleUser {
		t.Fatalf("second role = %q", second.User.Role)
	}
	if second.User.Username != "bob" {
		t.Fatalf("username = %q", second.User.Username)
	}
	if _, err := service.Register(ctx, "Alice", "password-one"); err == nil {
		t.Fatalf("duplicate username accepted")
	} else {
		assertAuthError(t, err, KindConflict, MessageUsernameTaken)
	}
}

func TestRegisterValidatesInput(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()

	_, err := service.Register(ctx, "ab", "password-one")
	assertAuthError(t, err, KindValidation, MessageUsernameInvalid)
	_, err = service.Register(ctx, "alice", "short")
	assertAuthError(t, err, KindValidation, MessagePasswordInvalid)
	_, err = service.Register(ctx, "-alice", "password-one")
	assertAuthError(t, err, KindValidation, MessageUsernameInvalid)
}

func TestRegistrationCanBeClosed(t *testing.T) {
	service, _ := newTestService(t, false, nil)
	_, err := service.Register(context.Background(), "alice", "password-one")
	assertAuthError(t, err, KindForbidden, MessageRegistrationClosed)
	if service.RegistrationEnabled() {
		t.Fatalf("RegistrationEnabled() = true")
	}
}

func TestLoginAndDisabledAccount(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()
	registered, err := service.Register(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := service.Login(ctx, "alice", "wrong-password"); err == nil {
		t.Fatalf("Login(wrong) succeeded")
	} else {
		assertAuthError(t, err, KindUnauthorized, MessageCredentials)
	}
	if _, err := service.Login(ctx, "missing", "password-one"); err == nil {
		t.Fatalf("Login(missing) succeeded")
	} else {
		assertAuthError(t, err, KindUnauthorized, MessageCredentials)
	}
	login, err := service.Login(ctx, "alice", "password-one")
	if err != nil || login.Token == "" || login.User.ID != registered.User.ID {
		t.Fatalf("Login() = %+v, %v", login, err)
	}

	// Disabling the account rejects both new logins and existing sessions. A
	// second administrator keeps the last-admin guard out of the way.
	second, err := service.Register(ctx, "bob", "password-two")
	if err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}
	if _, err := service.UpdateUser(ctx, second.User.ID, state.RoleAdmin, true); err != nil {
		t.Fatalf("UpdateUser(promote) error = %v", err)
	}
	if _, err := service.UpdateUser(ctx, registered.User.ID, state.RoleAdmin, false); err != nil {
		t.Fatalf("UpdateUser(disable) error = %v", err)
	}
	if _, err := service.Login(ctx, "alice", "password-one"); err == nil {
		t.Fatalf("Login(disabled) succeeded")
	} else {
		assertAuthError(t, err, KindUnauthorized, MessageAccountDisabled)
	}
	if session, err := service.Authenticate(ctx, login.Token); err == nil || session != nil {
		t.Fatalf("Authenticate(disabled) = %+v, %v", session, err)
	}
}

func TestSessionExpiryAndLogout(t *testing.T) {
	current := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	service, _ := newTestService(t, true, func() time.Time { return current })
	ctx := context.Background()

	login, err := service.Register(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := service.Authenticate(ctx, login.Token); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	current = current.Add(2 * time.Hour)
	if session, err := service.Authenticate(ctx, login.Token); err == nil || session != nil {
		t.Fatalf("Authenticate(expired) = %+v, %v", session, err)
	}

	current = current.Add(-2 * time.Hour)
	fresh, err := service.Login(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if err := service.Logout(ctx, fresh.Token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if session, err := service.Authenticate(ctx, fresh.Token); err == nil || session != nil {
		t.Fatalf("Authenticate(after logout) = %+v, %v", session, err)
	}
	// Logout is idempotent and tolerates unknown tokens.
	if err := service.Logout(ctx, "unknown"); err != nil {
		t.Fatalf("Logout(unknown) error = %v", err)
	}
	if err := service.Logout(ctx, ""); err != nil {
		t.Fatalf("Logout(empty) error = %v", err)
	}
}

func TestUpdateUserProtectsLastAdmin(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()
	admin, err := service.Register(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Register(admin) error = %v", err)
	}
	user, err := service.Register(ctx, "bob", "password-two")
	if err != nil {
		t.Fatalf("Register(user) error = %v", err)
	}

	if _, err := service.UpdateUser(ctx, admin.User.ID, state.RoleUser, true); err == nil {
		t.Fatalf("demoting the last admin succeeded")
	} else {
		assertAuthError(t, err, KindForbidden, MessageLastAdmin)
	}
	if _, err := service.UpdateUser(ctx, admin.User.ID, state.RoleAdmin, false); err == nil {
		t.Fatalf("disabling the last admin succeeded")
	} else {
		assertAuthError(t, err, KindForbidden, MessageLastAdmin)
	}
	if _, err := service.DeleteUser(ctx, admin.User.ID); err == nil {
		t.Fatalf("deleting the last admin succeeded")
	} else {
		assertAuthError(t, err, KindForbidden, MessageLastAdmin)
	}

	// Promoting the second account keeps the first one editable.
	if _, err := service.UpdateUser(ctx, user.User.ID, state.RoleAdmin, true); err != nil {
		t.Fatalf("UpdateUser(promote) error = %v", err)
	}
	updated, err := service.UpdateUser(ctx, admin.User.ID, state.RoleUser, true)
	if err != nil || updated.Role != state.RoleUser {
		t.Fatalf("UpdateUser(demote) = %+v, %v", updated, err)
	}

	if _, err := service.UpdateUser(ctx, "missing", state.RoleUser, true); err == nil {
		t.Fatalf("UpdateUser(missing) succeeded")
	} else {
		assertAuthError(t, err, KindNotFound, MessageUserNotFound)
	}
	if _, err := service.UpdateUser(ctx, user.User.ID, "owner", true); err == nil {
		t.Fatalf("UpdateUser(invalid role) succeeded")
	} else {
		assertAuthError(t, err, KindValidation, MessageRoleInvalid)
	}

	// Disabling an account revokes its live sessions.
	login, err := service.Login(ctx, "alice", "password-one")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if _, err := service.UpdateUser(ctx, updated.ID, state.RoleUser, false); err != nil {
		t.Fatalf("UpdateUser(disable) error = %v", err)
	}
	if session, err := service.Authenticate(ctx, login.Token); err == nil || session != nil {
		t.Fatalf("Authenticate(revoked by disable) = %+v, %v", session, err)
	}

	deleted, err := service.DeleteUser(ctx, updated.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteUser() = %v, %v", deleted, err)
	}
	deletedAgain, err := service.DeleteUser(ctx, updated.ID)
	if err == nil || deletedAgain {
		t.Fatalf("DeleteUser(again) = %v, %v", deletedAgain, err)
	} else {
		assertAuthError(t, err, KindNotFound, MessageUserNotFound)
	}
}

func TestUpdateUserAcceptsVIPRole(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()
	if _, err := service.Register(ctx, "alice", "password-one"); err != nil {
		t.Fatalf("Register(admin) error = %v", err)
	}
	bob, err := service.Register(ctx, "bob", "password-two")
	if err != nil {
		t.Fatalf("Register(user) error = %v", err)
	}
	updated, err := service.UpdateUser(ctx, bob.User.ID, state.RoleVIP, true)
	if err != nil {
		t.Fatalf("UpdateUser(vip) error = %v", err)
	}
	if updated == nil || updated.Role != state.RoleVIP || !updated.IsVIP() || updated.CanShare() != true {
		t.Fatalf("UpdateUser(vip) = %+v", updated)
	}
	if _, err := service.UpdateUser(ctx, bob.User.ID, "owner", true); err == nil {
		t.Fatalf("UpdateUser(invalid role) succeeded")
	} else {
		assertAuthError(t, err, KindValidation, MessageRoleInvalid)
	}
}

func TestListAndCountUsers(t *testing.T) {
	service, _ := newTestService(t, true, nil)
	ctx := context.Background()
	if _, err := service.Register(ctx, "alice", "password-one"); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := service.Register(ctx, "bob", "password-two"); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	users, err := service.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers() = %+v, %v", users, err)
	}
	count, err := service.CountUsers(ctx)
	if err != nil || count != 2 {
		t.Fatalf("CountUsers() = %d, %v", count, err)
	}
}
