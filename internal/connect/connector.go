package connect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ConnectorSpec is one declarative connector definition: a fixed name plus the
// Kafka Connect configuration. Templates live in deploy/kafka-connect/connectors
// and use ${NAME} / ${NAME:-default} placeholders so the same file works for
// compose, host processes and production.
type ConnectorSpec struct {
	Name   string
	Config map[string]string
	Source string
}

// placeholderPattern matches ${NAME} and ${NAME:-default}. Environment variable
// names follow the shell convention.
var placeholderPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// LoadConnectorFile parses and expands one connector template.
func LoadConnectorFile(path string, getenv func(string) string) (ConnectorSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ConnectorSpec{}, fmt.Errorf("read connector template %s: %w", filepath.Base(path), err)
	}
	var document struct {
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return ConnectorSpec{}, fmt.Errorf("parse connector template %s: %w", filepath.Base(path), err)
	}
	expandedName, err := expandString(document.Name, getenv)
	if err != nil {
		return ConnectorSpec{}, fmt.Errorf("%s: name: %w", filepath.Base(path), err)
	}
	if strings.TrimSpace(expandedName) == "" {
		return ConnectorSpec{}, fmt.Errorf("connector template %s has no name", filepath.Base(path))
	}
	config := make(map[string]string, len(document.Config))
	for key, value := range document.Config {
		text, ok := value.(string)
		if !ok {
			return ConnectorSpec{}, fmt.Errorf("connector template %s: config %q must be a string", filepath.Base(path), key)
		}
		expanded, expandErr := expandString(text, getenv)
		if expandErr != nil {
			return ConnectorSpec{}, fmt.Errorf("connector template %s: config %q: %w", filepath.Base(path), key, expandErr)
		}
		config[key] = expanded
	}
	return ConnectorSpec{
		Name:   strings.TrimSpace(expandedName),
		Config: config,
		Source: filepath.Base(path),
	}, nil
}

// LoadConnectorDir loads every *.json template in lexical order. The two fixed
// connector names must be distinct; a duplicate name or an unresolved
// placeholder fails before anything is sent to Kafka Connect.
func LoadConnectorDir(dir string, getenv func(string) string) ([]ConnectorSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read connector template directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("connector template directory %s contains no *.json files", dir)
	}
	specs := make([]ConnectorSpec, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		spec, loadErr := LoadConnectorFile(filepath.Join(dir, name), getenv)
		if loadErr != nil {
			return nil, loadErr
		}
		if previous, ok := seen[spec.Name]; ok {
			return nil, fmt.Errorf("connector name %q is declared by both %s and %s", spec.Name, previous, spec.Source)
		}
		seen[spec.Name] = spec.Source
		specs = append(specs, spec)
	}
	return specs, nil
}

// expandString substitutes every placeholder. A placeholder without a default
// requires the variable to be set: an empty MySQL password must fail the
// registration instead of producing a broken connector.
func expandString(text string, getenv func(string) string) (string, error) {
	var firstErr error
	expanded := placeholderPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := placeholderPattern.FindStringSubmatch(match)
		name := parts[1]
		fallback := parts[2]
		hasFallback := strings.Contains(match, ":-")
		value := getenv(name)
		if value == "" {
			if hasFallback {
				return fallback
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s is required", name)
			}
			return match
		}
		return value
	})
	if firstErr != nil {
		return "", firstErr
	}
	return expanded, nil
}
