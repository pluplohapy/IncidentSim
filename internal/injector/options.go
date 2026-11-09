package injector

import (
	"fmt"
	"strconv"
	"strings"
)

// Options is the typed view of a manifest fault's options.
//
// A manifest is YAML, so numbers arrive as float64 and booleans as bool. Every
// accessor normalises them once here, so an injector never has to know how the
// document was decoded.
type Options struct {
	raw map[string]any
}

// NewOptions wraps the raw options of a fault.
func NewOptions(raw map[string]any) Options { return Options{raw: raw} }

// Has reports whether a key is present, including when its value is empty.
func (o Options) Has(key string) bool {
	_, ok := o.raw[key]
	return ok
}

// String returns a string option or the fallback.
func (o Options) String(key, fallback string) string {
	value, ok := o.raw[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(value)
	}
}

// StringSlice returns a list option. A single scalar is a one element list, so
// a manifest does not have to write `- x` for a single value.
func (o Options) StringSlice(key string) []string {
	value, ok := o.raw[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return typed
	case string:
		return strings.Fields(typed)
	default:
		return []string{fmt.Sprint(value)}
	}
}

// Int returns an integer option or the fallback.
func (o Options) Int(key string, fallback int) int {
	value, ok := o.raw[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(typed)
		if err != nil {
			return fallback
		}
		return parsed
	default:
		return fallback
	}
}

// Bool returns a boolean option or the fallback.
func (o Options) Bool(key string, fallback bool) bool {
	value, ok := o.raw[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(typed)
		if err != nil {
			return fallback
		}
		return parsed
	case int:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return fallback
	}
}

// Duration returns a duration option or the fallback.
func (o Options) Duration(key string, fallback string) string {
	return o.String(key, fallback)
}

// Env returns the environment variables of an `env` option. This is the format
// every config-level fault uses, so it has a dedicated accessor.
func (o Options) Env() map[string]string {
	out := map[string]string{}
	env, ok := o.raw["env"]
	if !ok {
		return out
	}
	if variables, ok := env.(map[string]any); ok {
		for key, value := range variables {
			out[key] = fmt.Sprint(value)
		}
	}
	return out
}
