package mcp

import (
	"encoding/json"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Deterministic regression witness for issue 139. This asserts the documented
// Claude input-schema subset against the server's real tools/list response.
// It does not pretend to run Claude Code or validate output-schema support.
func TestIssue139InputSchemasCompatibleWithClaude(t *testing.T) {
	defs := listToolDefs(t)
	if len(defs) != 7 {
		t.Fatalf("tools/list advertised %d tools, want 7", len(defs))
	}
	for _, name := range []string{"memory_search", "memory_search_batch", "memory_add", "memory_alias", "checkpoint_save", "checkpoint_resume", "memory_reflect"} {
		t.Run(name, func(t *testing.T) {
			def, exists := defs[name]
			if !exists {
				t.Fatalf("tools/list omitted %s", name)
			}
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
				if _, present := schema[keyword]; present {
					t.Errorf("%s inputSchema has root %s; Claude deployments without schema rewriting exclude this tool", name, keyword)
				}
			}
			var objectType string
			if err := json.Unmarshal(schema["type"], &objectType); err != nil || objectType != "object" {
				t.Errorf("%s inputSchema must have object type, got %s", name, schema["type"])
			}
			compileInputSchema(t, def.InputSchema)
		})
	}
}

func compileInputSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode input schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const location = "memory://tool-input.json"
	if err := compiler.AddResource(location, document); err != nil {
		t.Fatalf("add input schema: %v", err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		t.Fatalf("compile input schema: %v", err)
	}
	return schema
}
