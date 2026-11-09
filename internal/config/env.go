package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// optionalEnv returns the trimmed value of an environment variable or "" when
// it is unset or blank. It mirrors rag_mcp.config._optional_env.
func optionalEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return ""
	}
	return value
}

// booleanEnv parses the same truthy/falsy spellings as the Python backend.
func booleanEnv(name string, defaultValue bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}

// integerEnv parses a bounded integer environment variable.
func integerEnv(name string, defaultValue, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		raw = strconv.Itoa(defaultValue)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

// intEnvDefault parses an integer with a fallback that is not range checked,
// matching the state settings helper in the Python implementation.
func intEnvDefault(name string, defaultValue int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		raw = strconv.Itoa(defaultValue)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}
