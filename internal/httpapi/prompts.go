package httpapi

import (
	"net/http"
	"sort"

	"github.com/Guan0923/Praxis-RAG/internal/catalog"
)

// promptBody mirrors catalog.PromptDefinition as accepted by the admin API.
type promptBody struct {
	Description string               `json:"description"`
	Arguments   []promptArgumentBody `json:"arguments"`
	Messages    []promptMessageBody  `json:"messages"`
}

type promptArgumentBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type promptMessageBody struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func (body promptBody) definition() catalog.PromptDefinition {
	arguments := make([]catalog.PromptArgument, 0, len(body.Arguments))
	for _, argument := range body.Arguments {
		arguments = append(arguments, catalog.PromptArgument{
			Name:        argument.Name,
			Description: argument.Description,
			Required:    argument.Required,
		})
	}
	messages := make([]catalog.PromptMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		messages = append(messages, catalog.PromptMessage{Role: message.Role, Text: message.Text})
	}
	return catalog.PromptDefinition{
		Description: body.Description,
		Arguments:   arguments,
		Messages:    messages,
	}
}

func (s *Server) catalogFor(namespace string) (*catalog.NamespaceCatalog, bool) {
	if s.catalog == nil {
		return nil, false
	}
	catalogForNamespace, err := catalog.NewNamespaceCatalog(s.catalog, namespace)
	if err != nil {
		return nil, false
	}
	return catalogForNamespace, true
}

func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	catalogForNamespace, ok := s.catalogFor(namespace)
	if !ok {
		writeDetail(w, http.StatusServiceUnavailable, "MCP configuration cannot be read")
		return
	}
	data, err := catalogForNamespace.Snapshot(r.Context(), false)
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "MCP configuration cannot be read")
		return
	}
	names := make([]string, 0, len(data.Prompts))
	for name := range data.Prompts {
		names = append(names, name)
	}
	sort.Strings(names)
	prompts := make([]map[string]any, 0, len(names))
	for _, name := range names {
		prompts = append(prompts, serializePrompt(name, data.Prompts[name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompts": prompts})
}

func serializePrompt(name string, prompt catalog.PromptDefinition) map[string]any {
	arguments := make([]map[string]any, 0, len(prompt.Arguments))
	for _, argument := range prompt.Arguments {
		arguments = append(arguments, map[string]any{
			"name":        argument.Name,
			"description": argument.Description,
			"required":    argument.Required,
		})
	}
	messages := make([]map[string]any, 0, len(prompt.Messages))
	for _, message := range prompt.Messages {
		messages = append(messages, map[string]any{"role": message.Role, "text": message.Text})
	}
	return map[string]any{
		"name":        name,
		"description": prompt.Description,
		"arguments":   arguments,
		"messages":    messages,
	}
}

func (s *Server) handlePutPrompt(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	name := pathParam(r, "name")
	if name == "" {
		writeDetail(w, http.StatusBadRequest, "Invalid prompt name")
		return
	}
	catalogForNamespace, ok := s.catalogFor(namespace)
	if !ok {
		writeDetail(w, http.StatusServiceUnavailable, "MCP configuration cannot be saved")
		return
	}
	var body promptBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	definition := body.definition()
	if err := definition.Validate(); err != nil {
		writeValidation(w, http.StatusUnprocessableEntity, []validationDetail{{
			Type: "value_error", Loc: []any{"body"}, Msg: err.Error(),
		}})
		return
	}
	changed, err := catalogForNamespace.PutPrompt(r.Context(), name, definition)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if changed && s.notifier != nil {
		s.notifier.PromptsChanged(namespace)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "changed": changed})
}

func (s *Server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.namespaceHeader(w, r)
	if !ok {
		return
	}
	name := pathParam(r, "name")
	catalogForNamespace, ok := s.catalogFor(namespace)
	if !ok {
		writeDetail(w, http.StatusServiceUnavailable, "MCP configuration cannot be saved")
		return
	}
	if err := catalogForNamespace.DeletePrompt(r.Context(), name); err != nil {
		var missing *catalog.MissingPromptError
		if asMissingPrompt(err, &missing) {
			writeDetail(w, http.StatusNotFound, "Unknown prompt.")
			return
		}
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.notifier != nil {
		s.notifier.PromptsChanged(namespace)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func asMissingPrompt(err error, target **catalog.MissingPromptError) bool {
	missing, ok := err.(*catalog.MissingPromptError)
	if ok {
		*target = missing
	}
	return ok
}
