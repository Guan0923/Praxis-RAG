package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// fakeUserAuth is an in-memory UserAuth for handler tests.
type fakeUserAuth struct {
	registration bool
	registerVal  auth.Login
	registerErr  error
	loginVal     auth.Login
	loginErr     error
	sessions     map[string]*auth.SessionUser
	users        []state.User
	updateVal    *state.User
	updateErr    error
	deleteVal    bool
	deleteErr    error
	loggedOut    []string
}

func (f *fakeUserAuth) RegistrationEnabled() bool { return f.registration }

func (f *fakeUserAuth) Register(context.Context, string, string) (auth.Login, error) {
	return f.registerVal, f.registerErr
}

func (f *fakeUserAuth) Login(context.Context, string, string) (auth.Login, error) {
	return f.loginVal, f.loginErr
}

func (f *fakeUserAuth) Logout(_ context.Context, token string) error {
	f.loggedOut = append(f.loggedOut, token)
	return nil
}

func (f *fakeUserAuth) Authenticate(_ context.Context, token string) (*auth.SessionUser, error) {
	session, ok := f.sessions[token]
	if !ok {
		return nil, &auth.Error{Kind: auth.KindUnauthorized, Message: auth.MessageSessionInvalid}
	}
	return session, nil
}

func (f *fakeUserAuth) ListUsers(context.Context) ([]state.User, error) {
	return f.users, nil
}

func (f *fakeUserAuth) UpdateUser(context.Context, string, string, bool) (*state.User, error) {
	return f.updateVal, f.updateErr
}

func (f *fakeUserAuth) DeleteUser(context.Context, string) (bool, error) {
	return f.deleteVal, f.deleteErr
}

func newAuthTestServer(t *testing.T, users UserAuth) *Server {
	t.Helper()
	settings := testSettings("")
	settings.AuthEnabled = true
	settings.RegistrationEnabled = true
	settings.SessionTTL = time.Hour
	return New(settings, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUserAuth(users),
	)
}

func adminSession(token string) *auth.SessionUser {
	enabled := state.User{
		ID: "u-admin", Username: "alice", Role: state.RoleAdmin, Enabled: true,
		CreatedAt: "2026-11-09T10:00:00.000000+00:00",
	}
	return &auth.SessionUser{User: enabled, CSRF: "csrf-" + token}
}

func userSession(token string) *auth.SessionUser {
	enabled := state.User{
		ID: "u-user", Username: "bob", Role: state.RoleUser, Enabled: true,
		CreatedAt: "2026-11-09T10:00:00.000000+00:00",
	}
	return &auth.SessionUser{User: enabled, CSRF: "csrf-" + token}
}

func cookieFor(token string) map[string]string {
	return map[string]string{"Cookie": sessionCookieName + "=" + token}
}

func TestAuthStatusReportsLegacyMode(t *testing.T) {
	handler := New(testSettings(""), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/auth/status", remote: "127.0.0.1:5000",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if body["auth_enabled"] != false || body["authenticated"] != true {
		t.Fatalf("legacy status = %v", body)
	}
}

func TestAuthRegisterSetsSessionCookie(t *testing.T) {
	now := time.Now().UTC()
	users := &fakeUserAuth{
		registration: true,
		registerVal: auth.Login{
			User:      state.User{ID: "u-admin", Username: "alice", Role: state.RoleAdmin, Enabled: true},
			Token:     "token-register",
			CSRF:      "csrf-register",
			ExpiresAt: now.Add(time.Hour),
		},
	}
	handler := newAuthTestServer(t, users).Handler()

	register := performRequest(handler, testRequest{
		method: "POST", target: "/v1/auth/register", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"username":"alice","password":"password-one"}`,
	})
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d body = %s", register.Code, register.Body.String())
	}
	body := decodeBody(t, register)
	if body["csrf_token"] != "csrf-register" {
		t.Fatalf("register body = %v", body)
	}
	user, _ := body["user"].(map[string]any)
	if user == nil || user["username"] != "alice" || user["role"] != state.RoleAdmin {
		t.Fatalf("register user = %v", body["user"])
	}

	cookies := register.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value != "token-register" ||
		!cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
		t.Fatalf("session cookie = %+v", cookie)
	}

	// The session cookie authenticates /v1/auth/me.
	users.sessions = map[string]*auth.SessionUser{"token-register": adminSession("token-register")}
	me := performRequest(handler, testRequest{
		method: "GET", target: "/v1/auth/me", remote: "127.0.0.1:5000",
		headers: cookieFor("token-register"),
	})
	if me.Code != http.StatusOK {
		t.Fatalf("me status = %d body = %s", me.Code, me.Body.String())
	}
	if body := decodeBody(t, me); body["csrf_token"] != "csrf-token-register" {
		t.Fatalf("me body = %v", body)
	}

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/auth/me", remote: "127.0.0.1:5000",
	})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous me status = %d", anonymous.Code)
	}
}

func TestAuthRegisterHonoursClosedRegistration(t *testing.T) {
	users := &fakeUserAuth{registration: false}
	handler := newAuthTestServer(t, users).Handler()
	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/auth/register", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"username":"alice","password":"password-one"}`,
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestAuthLoginRateLimited(t *testing.T) {
	users := &fakeUserAuth{loginErr: &auth.Error{Kind: auth.KindUnauthorized, Message: auth.MessageCredentials}}
	handler := newAuthTestServer(t, users).Handler()
	var last int
	for attempt := 0; attempt < loginAttemptLimit+1; attempt++ {
		response := performRequest(handler, testRequest{
			method: "POST", target: "/v1/auth/login", remote: "127.0.0.1:5000",
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"username":"alice","password":"wrong-password"}`,
		})
		last = response.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("final status = %d, want 429", last)
	}
}

func TestAuthAdminEndpointsRequireAdminAndCSRF(t *testing.T) {
	users := &fakeUserAuth{
		sessions: map[string]*auth.SessionUser{
			"admin-token": adminSession("admin-token"),
			"user-token":  userSession("user-token"),
		},
		users:     []state.User{{ID: "u-admin", Username: "alice", Role: state.RoleAdmin, Enabled: true}},
		deleteVal: true,
		updateVal: &state.User{
			ID: "u-user", Username: "bob", Role: state.RoleAdmin, Enabled: true,
		},
	}
	handler := newAuthTestServer(t, users).Handler()

	list := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/users", remote: "10.0.0.9:5000",
		headers: cookieFor("admin-token"),
	})
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s (remote admin sessions must work)", list.Code, list.Body.String())
	}

	noCSRF := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/users/u-user", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("admin-token"), map[string]string{"Content-Type": "application/json"}),
		body:    `{"role":"admin","enabled":true}`,
	})
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d body = %s", noCSRF.Code, noCSRF.Body.String())
	}

	withCSRF := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/users/u-user", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("admin-token"), map[string]string{
			"Content-Type": "application/json", csrfHeaderName: "csrf-admin-token",
		}),
		body: `{"role":"admin","enabled":true}`,
	})
	if withCSRF.Code != http.StatusOK {
		t.Fatalf("valid CSRF status = %d body = %s", withCSRF.Code, withCSRF.Body.String())
	}

	nonAdmin := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/users/u-user", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("user-token"), map[string]string{
			"Content-Type": "application/json", csrfHeaderName: "csrf-user-token",
		}),
		body: `{"role":"admin","enabled":true}`,
	})
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d body = %s", nonAdmin.Code, nonAdmin.Body.String())
	}

	// Bearer session tokens bypass CSRF (browsers cannot send them implicitly).
	bearer := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/admin/users/u-user", remote: "10.0.0.9:5000",
		headers: map[string]string{"Authorization": "Bearer admin-token"},
	})
	if bearer.Code != http.StatusNoContent {
		t.Fatalf("bearer delete status = %d body = %s", bearer.Code, bearer.Body.String())
	}
}

func TestAdminRejectsNonAdminEvenOnLoopback(t *testing.T) {
	users := &fakeUserAuth{
		sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")},
	}
	handler := newAuthTestServer(t, users).Handler()
	response := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/users", remote: "127.0.0.1:5000",
		headers: cookieFor("user-token"),
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("loopback non-admin status = %d body = %s", response.Code, response.Body.String())
	}
	// Anonymous loopback requests keep the break-glass access.
	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/admin/users", remote: "127.0.0.1:5000",
	})
	if anonymous.Code != http.StatusOK {
		t.Fatalf("anonymous loopback status = %d body = %s", anonymous.Code, anonymous.Body.String())
	}
}
func TestAuthTenantRoutesAcceptSession(t *testing.T) {
	users := &fakeUserAuth{
		sessions: map[string]*auth.SessionUser{"user-token": userSession("user-token")},
	}
	handler := newAuthTestServer(t, users).Handler()

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", remote: "10.0.0.9:5000",
		headers: map[string]string{"X-RAG-Namespace": "alpha"},
	})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", anonymous.Code)
	}

	authenticated := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", remote: "10.0.0.9:5000",
		headers: mergeHeaders(cookieFor("user-token"), map[string]string{"X-RAG-Namespace": "alpha"}),
	})
	if authenticated.Code != http.StatusOK {
		t.Fatalf("session status = %d body = %s", authenticated.Code, authenticated.Body.String())
	}

	missingNamespace := performRequest(handler, testRequest{
		method: "GET", target: "/v1/chunking/strategies", remote: "10.0.0.9:5000",
		headers: cookieFor("user-token"),
	})
	if missingNamespace.Code != http.StatusBadRequest {
		t.Fatalf("missing namespace status = %d", missingNamespace.Code)
	}
}

func TestAuthLogoutClearsCookie(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	handler := newAuthTestServer(t, users).Handler()
	response := performRequest(handler, testRequest{
		method: "POST", target: "/v1/auth/logout", remote: "127.0.0.1:5000",
		headers: cookieFor("admin-token"),
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d body = %s", response.Code, response.Body.String())
	}
	if len(users.loggedOut) != 1 || users.loggedOut[0] != "admin-token" {
		t.Fatalf("loggedOut = %v", users.loggedOut)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatalf("logout cookies = %v", cookies)
	}
}

func TestAuthUserUpdateValidation(t *testing.T) {
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{"admin-token": adminSession("admin-token")}}
	handler := newAuthTestServer(t, users).Handler()
	missingRole := performRequest(handler, testRequest{
		method: "PATCH", target: "/v1/admin/users/u-user", remote: "127.0.0.1:5000",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"enabled":true}`,
	})
	if missingRole.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing role status = %d body = %s", missingRole.Code, missingRole.Body.String())
	}
}

// mergeHeaders combines two header maps into a fresh map.
func mergeHeaders(first, second map[string]string) map[string]string {
	merged := make(map[string]string, len(first)+len(second))
	for key, value := range first {
		merged[key] = value
	}
	for key, value := range second {
		merged[key] = value
	}
	return merged
}

// Ensure the fake satisfies the interface at compile time.
var _ UserAuth = (*fakeUserAuth)(nil)
