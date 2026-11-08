package httpapi

import (
	"net/http"
	"sort"
	"strings"
)

// authPolicy selects which credential check protects a route.
type authPolicy int

const (
	authNone authPolicy = iota
	authTenant
	authAdmin
)

// route is one method+path registration. Patterns use "{name}" for a single
// path segment and a trailing "/*" for a prefix match.
type route struct {
	method   string
	pattern  string
	segments []segment
	prefix   string
	isPrefix bool
	handler  http.HandlerFunc
	policy   authPolicy
}

type segment struct {
	literal string
	param   string
}

// router is a small exact-semantics router: it distinguishes "no such path"
// (404) from "path exists but wrong method" (405), which net/http's ServeMux
// cannot do cleanly once a catch-all is registered.
type router struct {
	routes []*route
}

func newRouter() *router { return &router{} }

func (rt *router) add(method, pattern string, policy authPolicy, handler http.HandlerFunc) {
	item := &route{method: method, pattern: pattern, handler: handler, policy: policy}
	if strings.HasSuffix(pattern, "/*") {
		item.isPrefix = true
		item.prefix = strings.TrimSuffix(pattern, "*")
	} else {
		item.segments = splitPattern(pattern)
	}
	rt.routes = append(rt.routes, item)
}

func splitPattern(pattern string) []segment {
	trimmed := strings.Trim(pattern, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	segments := make([]segment, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			segments = append(segments, segment{param: part[1 : len(part)-1]})
			continue
		}
		segments = append(segments, segment{literal: part})
	}
	return segments
}

type matchResult struct {
	route  *route
	params map[string]string
}

// match returns the matched route and path params. pathMatched reports whether
// any route matches the path regardless of method.
func (rt *router) match(method, requestPath string) (result matchResult, pathMatched bool, allowed []string) {
	cleaned := cleanPath(requestPath)
	pathMatched = false
	for _, item := range rt.routes {
		params, ok := item.matchPath(cleaned)
		if !ok {
			continue
		}
		pathMatched = true
		if item.method == method {
			return matchResult{route: item, params: params}, true, nil
		}
		allowed = append(allowed, item.method)
	}
	sort.Strings(allowed)
	return matchResult{}, pathMatched, allowed
}

func (item *route) matchPath(requestPath string) (map[string]string, bool) {
	if item.isPrefix {
		if strings.HasPrefix(requestPath, item.prefix) || requestPath+"/" == item.prefix {
			return map[string]string{}, true
		}
		return nil, false
	}
	if requestPath == "/" {
		return map[string]string{}, len(item.segments) == 0
	}
	trimmed := strings.TrimPrefix(requestPath, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != len(item.segments) {
		return nil, false
	}
	params := make(map[string]string, 2)
	for index, expected := range item.segments {
		actual := parts[index]
		if expected.param != "" {
			if actual == "" {
				return nil, false
			}
			params[expected.param] = actual
			continue
		}
		if actual != expected.literal {
			return nil, false
		}
	}
	return params, true
}

// cleanPath normalizes a request path without touching URL-escaped segments.
func cleanPath(value string) string {
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	for len(value) > 1 && strings.HasSuffix(value, "/") {
		value = strings.TrimSuffix(value, "/")
	}
	return value
}
