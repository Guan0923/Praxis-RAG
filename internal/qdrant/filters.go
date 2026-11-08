package qdrant

import "sort"

// Filter is the Qdrant REST filter payload. Field order matches the Python
// pydantic model (should, min_should, must, must_not) for byte-stable bodies.
type Filter struct {
	Should    []any `json:"should,omitempty"`
	MinShould any   `json:"min_should,omitempty"`
	Must      []any `json:"must,omitempty"`
	MustNot   []any `json:"must_not,omitempty"`
}

// fieldCondition mirrors models.FieldCondition with only key+match set, which
// is all the Python store ever builds.
type fieldCondition struct {
	Key   string `json:"key"`
	Match any    `json:"match"`
}

type matchValue struct {
	Value any `json:"value"`
}

type matchAny struct {
	Any []any `json:"any"`
}

// isEmptyCondition mirrors models.IsEmptyCondition.
type isEmptyCondition struct {
	IsEmpty payloadField `json:"is_empty"`
}

type payloadField struct {
	Key string `json:"key"`
}

// payloadFilter builds Filter(must=[FieldCondition(key, MatchValue(value))...])
// with keys sorted like sorted(values.items()).
func payloadFilter(values map[string]any) *Filter {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	must := make([]any, 0, len(keys))
	for _, key := range keys {
		must = append(must, fieldCondition{Key: key, Match: matchValue{Value: values[key]}})
	}
	return &Filter{Must: must}
}

// scoped merges the namespace into explicit payload values.
func (s *Store) scoped(values map[string]any) map[string]any {
	scoped := make(map[string]any, len(values)+1)
	for key, value := range values {
		scoped[key] = value
	}
	if s.namespace != "" {
		scoped["namespace"] = s.namespace
	}
	return scoped
}

// versionScopeFilter restricts a query or delete to this store's namespace,
// adding an explicit is_empty(namespace) condition when no namespace is set.
func (s *Store) versionScopeFilter(values map[string]any) *Filter {
	queryFilter := payloadFilter(s.scoped(values))
	if s.namespace == "" {
		queryFilter.Must = append(queryFilter.Must, isEmptyCondition{IsEmpty: payloadField{Key: "namespace"}})
	}
	return queryFilter
}

// queryFilter adds the publication rule: exactly one active version per
// document, plus unversioned legacy points whose document is not head-tracked.
func (s *Store) queryFilter(values map[string]any) *Filter {
	active := map[string]string{}
	if s.activeVersions != nil {
		active = s.activeVersions()
	}
	legacy := &Filter{Must: []any{isEmptyCondition{IsEmpty: payloadField{Key: "upload_version"}}}}
	if len(active) > 0 {
		documentIDs := make([]any, 0, len(active))
		for _, documentID := range sortedKeys(active) {
			documentIDs = append(documentIDs, documentID)
		}
		legacy.MustNot = []any{fieldCondition{Key: "document_id", Match: matchAny{Any: documentIDs}}}
	}
	publication := &Filter{Should: make([]any, 0, len(active)+1)}
	for _, documentID := range sortedKeys(active) {
		version := active[documentID]
		if version == "" {
			continue
		}
		publication.Should = append(publication.Should, payloadFilter(map[string]any{
			"document_id":    documentID,
			"upload_version": version,
		}))
	}
	publication.Should = append(publication.Should, legacy)
	queryFilter := s.versionScopeFilter(values)
	queryFilter.Must = append(queryFilter.Must, publication)
	return queryFilter
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
