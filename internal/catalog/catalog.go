// Package catalog holds namespace-local MCP tool settings and inert prompt
// templates. It is a port of rag_mcp/catalog.py; persistence is delegated to a
// Store so the state layer can own the database transaction.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// NamePattern is the shared namespace/tool/prompt identifier format.
const NamePattern = `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	placeholder = regexp.MustCompile(`\{\{([A-Za-z0-9_.-]+)\}\}`)
)

// Error is a safe, user-facing catalog failure.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// ErrConfigurationRead and ErrConfigurationWrite are wrapped by safe errors so
// the HTTP layer can map them to 503 like the Python service.
var (
	ErrConfigurationRead  = &Error{Message: "MCP configuration cannot be read"}
	ErrConfigurationWrite = &Error{Message: "MCP configuration cannot be saved"}
)

// ToolPatch is a partial update of one registered tool's settings.
type ToolPatch struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	Description *string `json:"description,omitempty"`
}

// ToolSettings is the persisted form of a tool override.
type ToolSettings struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	Description *string `json:"description,omitempty"`
}

// PromptArgument describes one prompt template argument.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is one inert prompt message. Role is "user" or "assistant".
type PromptMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// PromptDefinition is one stored prompt template.
type PromptDefinition struct {
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
	Messages    []PromptMessage  `json:"messages"`
}

// RenderedMessage is the substituted form returned by MCP get_prompt.
type RenderedMessage struct {
	Role string
	Text string
}

// Data is one namespace's persisted catalog document.
type Data struct {
	Tools   map[string]ToolSettings     `json:"tools"`
	Prompts map[string]PromptDefinition `json:"prompts"`
}

// NewData returns an empty catalog with initialized maps so JSON output always
// contains {"tools":{},"prompts":{}} like CatalogData.model_dump_json.
func NewData() *Data {
	return &Data{
		Tools:   map[string]ToolSettings{},
		Prompts: map[string]PromptDefinition{},
	}
}

// Store persists raw catalog JSON per namespace. The state layer implements it
// with the shared database; MemoryStore is the in-process implementation.
type Store interface {
	// Load returns the persisted JSON document or nil when none exists.
	Load(ctx context.Context, namespace string) ([]byte, error)
	// Mutate runs transform inside the store's transaction and persists the
	// result when transform reports changed. It returns the resulting catalog.
	Mutate(ctx context.Context, namespace string, transform func(*Data) (changed bool, err error)) (*Data, error)
}

// MemoryStore is a mutex-guarded in-memory Store used by tests and previews.
type MemoryStore struct {
	documents map[string][]byte
}

// NewMemoryStore creates an empty in-memory catalog store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{documents: map[string][]byte{}}
}

// Load implements Store.
func (s *MemoryStore) Load(_ context.Context, namespace string) ([]byte, error) {
	raw, ok := s.documents[namespace]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), raw...), nil
}

// Mutate implements Store.
func (s *MemoryStore) Mutate(_ context.Context, namespace string, transform func(*Data) (bool, error)) (*Data, error) {
	data := NewData()
	if raw, ok := s.documents[namespace]; ok && len(raw) > 0 {
		decoded, err := Decode(raw)
		if err != nil {
			return nil, err
		}
		data = decoded
	}
	changed, err := transform(data)
	if err != nil {
		return nil, err
	}
	if changed {
		encoded, err := Encode(data)
		if err != nil {
			return nil, err
		}
		s.documents[namespace] = encoded
	}
	return data, nil
}

// Decode parses and validates a persisted catalog document.
func Decode(raw []byte) (*Data, error) {
	data := NewData()
	if err := json.Unmarshal(raw, data); err != nil {
		return nil, fmt.Errorf("invalid MCP configuration JSON: %w", err)
	}
	if data.Tools == nil {
		data.Tools = map[string]ToolSettings{}
	}
	if data.Prompts == nil {
		data.Prompts = map[string]PromptDefinition{}
	}
	if err := data.Validate(); err != nil {
		return nil, err
	}
	return data, nil
}

// Encode serializes a catalog document deterministically (indented, like
// CatalogData.model_dump_json(indent=2)).
func Encode(data *Data) ([]byte, error) {
	return json.MarshalIndent(data, "", "  ")
}

// Validate enforces the same persisted-settings constraints as CatalogData.
func (d *Data) Validate() error {
	for name, settings := range d.Tools {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("Invalid registered tool name")
		}
		if err := settings.validate(); err != nil {
			return err
		}
	}
	for name := range d.Prompts {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("Invalid prompt name")
		}
	}
	return nil
}

func (s ToolSettings) validate() error {
	if s.Enabled == nil && s.Description == nil {
		return fmt.Errorf("Provide enabled and/or description; null is not accepted")
	}
	return nil
}

// Validate rejects a ToolPatch that supplies nothing or wrong-shaped values.
func (p ToolPatch) Validate() error {
	if p.Enabled == nil && p.Description == nil {
		return fmt.Errorf("Provide enabled and/or description; null is not accepted")
	}
	if p.Description != nil && len([]rune(*p.Description)) > 8000 {
		return fmt.Errorf("String should have at most 8000 characters")
	}
	return nil
}

// Validate checks one prompt definition.
func (p PromptDefinition) Validate() error {
	if len([]rune(p.Description)) > 8000 {
		return fmt.Errorf("String should have at most 8000 characters")
	}
	if len(p.Arguments) > 32 {
		return fmt.Errorf("List should have at most 32 items")
	}
	names := make([]string, 0, len(p.Arguments))
	seen := map[string]struct{}{}
	for _, argument := range p.Arguments {
		if !namePattern.MatchString(argument.Name) {
			return fmt.Errorf("String should match pattern %q", NamePattern)
		}
		if _, duplicate := seen[argument.Name]; duplicate {
			return fmt.Errorf("Prompt arguments must have unique names")
		}
		seen[argument.Name] = struct{}{}
		names = append(names, argument.Name)
		if len([]rune(argument.Description)) > 2000 {
			return fmt.Errorf("String should have at most 2000 characters")
		}
	}
	if len(p.Messages) < 1 {
		return fmt.Errorf("List should have at least 1 item")
	}
	if len(p.Messages) > 32 {
		return fmt.Errorf("List should have at most 32 items")
	}
	for _, message := range p.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return fmt.Errorf("Input should be 'user' or 'assistant'")
		}
		if len([]rune(message.Text)) > 32000 {
			return fmt.Errorf("String should have at most 32000 characters")
		}
		declared := map[string]struct{}{}
		for _, name := range names {
			declared[name] = struct{}{}
		}
		for _, match := range placeholder.FindAllStringSubmatch(message.Text, -1) {
			if _, ok := declared[match[1]]; !ok {
				return fmt.Errorf("Prompt placeholders must refer to declared arguments")
			}
		}
		remaining := placeholder.ReplaceAllString(message.Text, "")
		if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
			return fmt.Errorf("Use {{argument}} for prompt placeholders")
		}
	}
	return nil
}

// Render substitutes declared arguments into the prompt messages.
func (p PromptDefinition) Render(arguments map[string]string) ([]RenderedMessage, error) {
	declared := map[string]struct{}{}
	for _, argument := range p.Arguments {
		declared[argument.Name] = struct{}{}
	}
	for name := range arguments {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("Unknown or non-string prompt argument")
		}
	}
	missing := make([]string, 0)
	for _, argument := range p.Arguments {
		if _, provided := arguments[argument.Name]; argument.Required && !provided {
			missing = append(missing, argument.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Missing prompt arguments: %s", strings.Join(missing, ", "))
	}
	rendered := make([]RenderedMessage, 0, len(p.Messages))
	for _, message := range p.Messages {
		text := placeholder.ReplaceAllStringFunc(message.Text, func(match string) string {
			groups := placeholder.FindStringSubmatch(match)
			if value, ok := arguments[groups[1]]; ok {
				return value
			}
			return ""
		})
		rendered = append(rendered, RenderedMessage{Role: message.Role, Text: text})
	}
	return rendered, nil
}

// NamespaceCatalog is one namespace's tool/prompt settings.
type NamespaceCatalog struct {
	store     Store
	namespace string
}

// NewNamespaceCatalog binds a store to a namespace.
func NewNamespaceCatalog(store Store, namespace string) (*NamespaceCatalog, error) {
	if !namePattern.MatchString(namespace) {
		return nil, fmt.Errorf("Invalid namespace")
	}
	return &NamespaceCatalog{store: store, namespace: namespace}, nil
}

// Snapshot reads the catalog. authoritative bypasses any read-model cache; the
// Go store currently has no cache, so both paths load from the store.
func (c *NamespaceCatalog) Snapshot(ctx context.Context, authoritative bool) (*Data, error) {
	_ = authoritative
	raw, err := c.store.Load(ctx, c.namespace)
	if err != nil {
		return nil, ErrConfigurationRead
	}
	if raw == nil {
		return NewData(), nil
	}
	data, err := Decode(raw)
	if err != nil {
		return nil, ErrConfigurationRead
	}
	return data, nil
}

// PatchTool applies a tool override. It returns whether the catalog changed.
func (c *NamespaceCatalog) PatchTool(ctx context.Context, name string, patch ToolPatch, defaults ToolSettings) (bool, error) {
	if err := patch.Validate(); err != nil {
		return false, err
	}
	changed := false
	_, err := c.mutate(ctx, func(data *Data) (bool, error) {
		previous := mergeSettings(defaults, data.Tools[name])
		updated := mergeSettings(previous, ToolSettings{Enabled: patch.Enabled, Description: patch.Description})
		if equality(previous, updated) {
			return false, nil
		}
		data.Tools[name] = updated
		changed = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// PutPrompt stores a prompt template and reports whether it changed.
func (c *NamespaceCatalog) PutPrompt(ctx context.Context, name string, prompt PromptDefinition) (bool, error) {
	if !namePattern.MatchString(name) {
		return false, fmt.Errorf("Invalid prompt name")
	}
	if err := prompt.Validate(); err != nil {
		return false, err
	}
	changed := false
	_, err := c.mutate(ctx, func(data *Data) (bool, error) {
		if existing, ok := data.Prompts[name]; ok && promptsEqual(existing, prompt) {
			return false, nil
		}
		data.Prompts[name] = prompt
		changed = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// DeletePrompt removes a prompt template. A missing prompt is a KeyError.
func (c *NamespaceCatalog) DeletePrompt(ctx context.Context, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("Invalid prompt name")
	}
	_, err := c.mutate(ctx, func(data *Data) (bool, error) {
		if _, ok := data.Prompts[name]; !ok {
			return false, &MissingPromptError{Name: name}
		}
		delete(data.Prompts, name)
		return true, nil
	})
	if err != nil {
		var missing *MissingPromptError
		if errors.As(err, &missing) {
			return missing
		}
		return err
	}
	return nil
}

// MissingPromptError mirrors the Python KeyError raised by delete_prompt.
type MissingPromptError struct{ Name string }

func (e *MissingPromptError) Error() string { return fmt.Sprintf("missing prompt: %s", e.Name) }

func (c *NamespaceCatalog) mutate(ctx context.Context, transform func(*Data) (bool, error)) (*Data, error) {
	data, err := c.store.Mutate(ctx, c.namespace, transform)
	if err != nil {
		var missing *MissingPromptError
		if errors.As(err, &missing) {
			return nil, err
		}
		return nil, ErrConfigurationWrite
	}
	return data, nil
}

func mergeSettings(previous, patch ToolSettings) ToolSettings {
	merged := previous
	if patch.Enabled != nil {
		merged.Enabled = patch.Enabled
	}
	if patch.Description != nil {
		merged.Description = patch.Description
	}
	return merged
}

func equality(left, right ToolSettings) bool {
	return boolEqual(left.Enabled, right.Enabled) && stringEqual(left.Description, right.Description)
}

func boolEqual(left, right *bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func stringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func promptsEqual(left, right PromptDefinition) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}
