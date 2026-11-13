package httpapi

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/config"
)

// namespacePattern is the tenant identifier format shared by every entry point.
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// KeyLookup resolves the namespaces authorized for one API key secret.
type KeyLookup interface {
	NamespacesForKey(ctx context.Context, secret string) ([]string, error)
}

// namespaceFromHeader validates and returns the X-RAG-Namespace header value.
func namespaceFromHeader(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("X-RAG-Namespace"))
	if !namespacePattern.MatchString(value) {
		return "", false
	}
	return value, true
}

// tenantIdentity is the authenticated caller of one tenant route. session is set
// for Console login sessions; keyNamespaces carries the namespaces bound to an
// API-key credential (kept from the authentication lookup so handlers such as
// the namespace list can filter without resolving the secret twice).
type tenantIdentity struct {
	namespace     string
	session       *auth.SessionUser
	keyNamespaces []string
}

// tenantFromHeaders resolves the tenant namespace for a request. It returns the
// namespace, or writes the HTTP error and returns ok=false.
func (s *Server) tenantFromHeaders(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := s.tenantFromRequest(w, r)
	return identity.namespace, ok
}

// tenantFromRequest is tenantFromHeaders plus the credential identity: the login
// session for Console sessions, or the namespaces an API key is bound to.
func (s *Server) tenantFromRequest(w http.ResponseWriter, r *http.Request) (tenantIdentity, bool) {
	return s.tenantFromRequestWith(w, r, false)
}

// tenantFromListingRequest authenticates GET /v1/namespaces: the endpoint
// reports the credential's own accessible scope, so it must not require an
// X-RAG-Namespace that is still authorized. Otherwise a rotated API key (or a
// namespace remembered from another account in legacy Console mode) could never
// read the list again and the Console could not recover. Credential checks are
// identical to tenantFromRequest; only the namespace header is optional and,
// when present, ignored.
func (s *Server) tenantFromListingRequest(w http.ResponseWriter, r *http.Request) (tenantIdentity, bool) {
	return s.tenantFromRequestWith(w, r, true)
}

// tenantFromRequestWith implements both tenant authentication variants.
func (s *Server) tenantFromRequestWith(w http.ResponseWriter, r *http.Request, namespaceOptional bool) (tenantIdentity, bool) {
	authorization := r.Header.Get("Authorization")

	// Console login sessions (cookies, or a bearer session token) take
	// precedence over the legacy service token and per-namespace API keys.
	if s.authEnabled() {
		if token, viaCookie := s.sessionTokenFromRequest(r); token != "" {
			session, err := s.users.Authenticate(r.Context(), token)
			if err == nil && session != nil {
				if !s.validateCSRF(r, session, viaCookie) {
					writeDetail(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面后重试")
					return tenantIdentity{}, false
				}
				if namespaceOptional {
					return tenantIdentity{session: session}, true
				}
				namespace, ok := namespaceFromHeader(r)
				if !ok {
					writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
					return tenantIdentity{}, false
				}
				return tenantIdentity{namespace: namespace, session: session}, true
			}
		}
	}

	if s.settings.Token != "" && secureEqual(authorization, "Bearer "+s.settings.Token) {
		if namespaceOptional {
			return tenantIdentity{}, true
		}
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return tenantIdentity{}, false
		}
		return tenantIdentity{namespace: namespace}, true
	}

	if strings.HasPrefix(authorization, "Bearer ") {
		secret := strings.TrimPrefix(authorization, "Bearer ")
		if s.keys == nil {
			writeDetail(w, http.StatusUnauthorized, "RAG service authentication failed.")
			return tenantIdentity{}, false
		}
		namespaces, err := s.keys.NamespacesForKey(r.Context(), secret)
		if err != nil {
			writeDetail(w, http.StatusUnauthorized, err.Error())
			return tenantIdentity{}, false
		}
		// The list endpoint answers with the credential's binding itself: a
		// stale header is ignored instead of rejected so a rotated key can
		// recover through the Console.
		if namespaceOptional {
			return tenantIdentity{keyNamespaces: namespaces}, true
		}
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return tenantIdentity{}, false
		}
		for _, candidate := range namespaces {
			if candidate == namespace {
				return tenantIdentity{namespace: namespace, keyNamespaces: namespaces}, true
			}
		}
		writeDetail(w, http.StatusForbidden, "API Key 未获授权访问此 Namespace")
		return tenantIdentity{}, false
	}

	if authorization == "" && s.settings.Token == "" && localModelRequest(r) {
		if namespaceOptional {
			return tenantIdentity{}, true
		}
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return tenantIdentity{}, false
		}
		return tenantIdentity{namespace: namespace}, true
	}

	writeDetail(w, http.StatusUnauthorized, "RAG service authentication failed.")
	return tenantIdentity{}, false
}

// requireAdmin protects the management endpoints. An explicit login session is
// always checked against its role (a normal user cannot borrow loopback trust);
// requests without a session keep the local break-glass access used by dev
// machine automation.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.authEnabled() {
		if session, viaCookie := s.requestSessionUser(r); session != nil {
			if !s.validateCSRF(r, session, viaCookie) {
				writeDetail(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面后重试")
				return false
			}
			if !session.User.IsAdmin() {
				writeDetail(w, http.StatusForbidden, "需要管理员权限")
				return false
			}
			return true
		}
	}
	if localModelRequest(r) {
		return true
	}
	writeDetail(w, http.StatusForbidden, "仅允许本机直接访问管理接口")
	return false
}

func secureEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// localModelRequest mirrors http_service._local_model_request: the request must
// come from loopback, address a loopback host, carry no proxy headers, and have
// a same-origin Origin when one is present.
func localModelRequest(r *http.Request) bool {
	clientHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientHost = r.RemoteAddr
	}
	clientIP := net.ParseIP(strings.Trim(clientHost, "[]"))
	if clientIP == nil || !clientIP.IsLoopback() {
		return false
	}

	hostname := r.Host
	if parsed, parseErr := url.Parse("//" + r.Host); parseErr == nil {
		hostname = parsed.Hostname()
	}
	if _, ok := config.LoopbackHosts[hostname]; !ok {
		return false
	}

	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		if r.Header.Get(header) != "" {
			return false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil || parsed.Scheme == "" {
			return false
		}
		requestScheme := "http"
		if r.TLS != nil {
			requestScheme = "https"
		}
		if parsed.Scheme != requestScheme || parsed.User != nil {
			return false
		}
		originHost := parsed.Hostname()
		if originHost != hostname {
			return false
		}
		targetPort := portOrDefault(r.Host, requestScheme)
		originPort := portOrDefault(parsed.Host, parsed.Scheme)
		if originPort != targetPort {
			return false
		}
	}
	return true
}

func portOrDefault(hostport, scheme string) string {
	parsed, err := url.Parse("//" + hostport)
	if err == nil {
		if port := parsed.Port(); port != "" {
			return port
		}
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}
