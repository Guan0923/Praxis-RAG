package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// validationDetail mirrors FastAPI's error detail item shape.
type validationDetail struct {
	Type string `json:"type"`
	Loc  []any  `json:"loc"`
	Msg  string `json:"msg"`
}

// decodeJSONBody decodes exactly one JSON object with unknown-field rejection.
// On failure it returns FastAPI-shaped validation details for a 422 response.
func decodeJSONBody(r *http.Request, target any) []validationDetail {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return []validationDetail{{Type: "missing", Loc: []any{"body"}, Msg: "Field required"}}
		}
		var syntaxError *json.SyntaxError
		if errors.As(err, &syntaxError) {
			return []validationDetail{{Type: "json_invalid", Loc: []any{"body"}, Msg: "JSON decode error"}}
		}
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return []validationDetail{{
				Type: "extra_forbidden",
				Loc:  []any{"body", field},
				Msg:  "Extra inputs are not permitted",
			}}
		}
		return []validationDetail{{Type: "json_invalid", Loc: []any{"body"}, Msg: err.Error()}}
	}
	if decoder.More() {
		return []validationDetail{{Type: "json_invalid", Loc: []any{"body"}, Msg: "Trailing data"}}
	}
	return nil
}

// writeValidation writes the FastAPI 422/400 validation envelope.
func writeValidation(w http.ResponseWriter, status int, details []validationDetail) {
	writeJSON(w, status, map[string]any{"detail": details})
}

// pathParam returns the router path parameter captured for the request.
func pathParam(r *http.Request, name string) string {
	params, _ := r.Context().Value(pathParamsContext{}).(map[string]string)
	return params[name]
}

// namespaceHeader validates the tenant header used by admin routes.
func (s *Server) namespaceHeader(w http.ResponseWriter, r *http.Request) (string, bool) {
	namespace, ok := namespaceFromHeader(r)
	if !ok {
		writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
		return "", false
	}
	return namespace, true
}
