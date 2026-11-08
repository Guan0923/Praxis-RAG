package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

// namespacePattern is the tenant identifier format shared by every entry point.
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// KeyLookup resolves the namespaces authorized for one API key secret.
type KeyLookup interface {
	NamespacesForKey(secret string) ([]string, error)
}

// namespaceFromHeader validates and returns the X-RAG-Namespace header value.
func namespaceFromHeader(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("X-RAG-Namespace"))
	if !namespacePattern.MatchString(value) {
		return "", false
	}
	return value, true
}

// tenantFromHeaders resolves the tenant namespace for a request. It returns the
// namespace, or writes the HTTP error and returns ok=false.
func (s *Server) tenantFromHeaders(w http.ResponseWriter, r *http.Request) (string, bool) {
	authorization := r.Header.Get("Authorization")

	if s.settings.Token != "" && secureEqual(authorization, "Bearer "+s.settings.Token) {
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return "", false
		}
		return namespace, true
	}

	if strings.HasPrefix(authorization, "Bearer ") {
		secret := strings.TrimPrefix(authorization, "Bearer ")
		if s.keys == nil {
			writeDetail(w, http.StatusUnauthorized, "RAG service authentication failed.")
			return "", false
		}
		namespaces, err := s.keys.NamespacesForKey(secret)
		if err != nil {
			writeDetail(w, http.StatusUnauthorized, err.Error())
			return "", false
		}
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return "", false
		}
		for _, candidate := range namespaces {
			if candidate == namespace {
				return namespace, true
			}
		}
		writeDetail(w, http.StatusForbidden, "API Key 未获授权访问此 Namespace")
		return "", false
	}

	if authorization == "" && s.settings.Token == "" && localModelRequest(r) {
		namespace, ok := namespaceFromHeader(r)
		if !ok {
			writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
			return "", false
		}
		return namespace, true
	}

	writeDetail(w, http.StatusUnauthorized, "RAG service authentication failed.")
	return "", false
}

// requireLocalAdmin enforces the local-only management boundary.
func (s *Server) requireLocalAdmin(w http.ResponseWriter, r *http.Request) bool {
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
