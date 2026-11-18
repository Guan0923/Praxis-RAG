package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
)

// searchBody mirrors http_service.SearchBody. Unknown fields are ignored like
// pydantic's default model_config.
type searchBody struct {
	Query      *string                    `json:"query"`
	Retrieval  *string                    `json:"retrieval"`
	Rerank     *string                    `json:"rerank"`
	CandidateK *json.Number               `json:"candidate_k"`
	TopK       *json.Number               `json:"top_k"`
	Filters    map[string]json.RawMessage `json:"filters"`
}

// decodeSearchBody decodes the search body with number preservation and
// without unknown-field rejection.
func decodeSearchBody(r *http.Request) (searchBody, []validationDetail) {
	var body searchBody
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		if err == io.EOF {
			return searchBody{}, []validationDetail{{Type: "missing", Loc: []any{"body"}, Msg: "Field required"}}
		}
		return searchBody{}, []validationDetail{{Type: "json_invalid", Loc: []any{"body"}, Msg: "JSON decode error"}}
	}
	return body, nil
}

// toArguments converts the body to the MCP-style argument map consumed by
// contracts.SearchRequestFromArguments.
func (b searchBody) toArguments() (map[string]any, []validationDetail) {
	arguments := map[string]any{}
	if b.Query == nil {
		return nil, []validationDetail{{Type: "missing", Loc: []any{"body", "query"}, Msg: "Field required"}}
	}
	arguments["query"] = *b.Query
	if b.Retrieval != nil {
		arguments["retrieval"] = *b.Retrieval
	}
	if b.Rerank != nil {
		switch *b.Rerank {
		case string(contracts.RerankNone), string(contracts.RerankCrossEncoder):
			arguments["rerank"] = *b.Rerank
		default:
			return nil, []validationDetail{{
				Type: "literal_error", Loc: []any{"body", "rerank"},
				Msg: "Input should be 'none' or 'cross_encoder'",
			}}
		}
	}
	if b.CandidateK != nil {
		value, err := strictJSONInteger(*b.CandidateK)
		if err != nil {
			return nil, []validationDetail{{Type: "int_parsing", Loc: []any{"body", "candidate_k"}, Msg: "Input should be a valid integer"}}
		}
		arguments["candidate_k"] = value
	}
	if b.TopK != nil {
		value, err := strictJSONInteger(*b.TopK)
		if err != nil {
			return nil, []validationDetail{{Type: "int_parsing", Loc: []any{"body", "top_k"}, Msg: "Input should be a valid integer"}}
		}
		arguments["top_k"] = value
	}
	if b.Filters != nil {
		filters := make(map[string]any, len(b.Filters))
		for key, raw := range b.Filters {
			value, ok := decodeFilterValue(raw)
			if !ok {
				return nil, []validationDetail{{
					Type: "union_tag_invalid", Loc: []any{"body", "filters", key},
					Msg: "Input should be a valid string, number, or boolean",
				}}
			}
			filters[key] = value
		}
		arguments["filters"] = filters
	}
	return arguments, nil
}

// decodeFilterValue accepts only JSON strings, booleans and finite numbers.
func decodeFilterValue(raw json.RawMessage) (any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, false
		}
		return value, true
	case 't', 'f':
		var value bool
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, false
		}
		return value, true
	case 'n':
		return nil, false
	default:
		number := json.Number(trimmed)
		if _, err := number.Float64(); err != nil {
			return nil, false
		}
		if integer, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return integer, true
		}
		float, err := number.Float64()
		if err != nil {
			return nil, false
		}
		return float, true
	}
}

// strictJSONInteger converts a JSON number token that must be an integer.
func strictJSONInteger(number json.Number) (int, error) {
	return strconv.Atoi(string(number))
}

func parseIntStrict(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty integer")
	}
	return strconv.Atoi(raw)
}

func itoa(value int) string { return strconv.Itoa(value) }
