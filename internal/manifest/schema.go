package manifest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

//go:embed schema.json
var schemaSource []byte

// Issue is a single validation problem with a machine readable location.
type Issue struct {
	// Field is a dotted path into the manifest, e.g. spec.services[1].name.
	Field string
	// Message explains what is wrong and, where possible, how to fix it.
	Message string
}

// ValidationError aggregates schema and semantic issues of one manifest.
type ValidationError struct {
	File   string
	Issues []Issue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return fmt.Sprintf("manifest %s: invalid", e.File)
	}
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, fmt.Sprintf("%s: %s", issue.Field, issue.Message))
	}
	return fmt.Sprintf("manifest %s is invalid: %s", e.File, strings.Join(parts, "; "))
}

var (
	compiledSchema    *jsonschema.Schema
	compiledSchemaErr error
	compileSchemaOnce sync.Once
)

// Schema returns the compiled JSON Schema of the manifest format.
func Schema() (*jsonschema.Schema, error) {
	compileSchemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("schema.json", bytes.NewReader(schemaSource)); err != nil {
			compiledSchemaErr = fmt.Errorf("add embedded schema: %w", err)
			return
		}
		compiledSchema, compiledSchemaErr = compiler.Compile("schema.json")
	})
	return compiledSchema, compiledSchemaErr
}

// validateAgainstSchema checks the YAML document against the embedded JSON
// Schema and returns concrete issues instead of a boolean.
func validateAgainstSchema(file string, data []byte) []Issue {
	schema, err := Schema()
	if err != nil {
		return []Issue{{Field: "<schema>", Message: err.Error()}}
	}

	doc, err := yamlToJSON(data)
	if err != nil {
		return []Issue{{Field: "<document>", Message: fmt.Sprintf("not a YAML mapping: %v", err)}}
	}

	verr := schema.Validate(doc)
	if verr == nil {
		return nil
	}

	var issues []Issue
	if detailed, ok := verr.(*jsonschema.ValidationError); ok {
		issues = append(issues, leaves(detailed, nil)...)
	} else {
		issues = append(issues, Issue{Field: "<document>", Message: verr.Error()})
	}
	return issues
}

// leaves flattens the validation error tree. Only the deepest causes carry a
// useful message for a manifest author, so the wrapper keywords are skipped.
func leaves(err *jsonschema.ValidationError, acc []Issue) []Issue {
	if len(err.Causes) == 0 {
		return append(acc, Issue{
			Field:   jsonPointerToField(err.InstanceLocation),
			Message: err.Message,
		})
	}
	for _, cause := range err.Causes {
		acc = leaves(cause, acc)
	}
	return acc
}

// yamlToJSON converts a YAML document into the generic value shape the JSON
// Schema validator works with.
func yamlToJSON(data []byte) (any, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("document is empty")
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, fmt.Errorf("document must be a mapping, got %T", doc)
	}
	normalised, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(normalised, &generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// jsonPointerToField turns a JSON Pointer instance location into the dotted
// field notation used by Issue.Field.
func jsonPointerToField(pointer string) string {
	if pointer == "" || pointer == "/" {
		return "<root>"
	}
	trimmed := strings.TrimPrefix(pointer, "/")
	trimmed = strings.ReplaceAll(trimmed, "/", ".")
	trimmed = strings.ReplaceAll(trimmed, "~1", "/")
	trimmed = strings.ReplaceAll(trimmed, "~0", "~")
	return trimmed
}

// sortedIssues makes error output stable for tests and humans alike.
func sortedIssues(issues []Issue) []Issue {
	out := make([]Issue, len(issues))
	copy(out, issues)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Message < out[j].Message
	})
	return out
}
