package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestMemoryReflect_AgentIDCanonical pins issue #77 step 3 (the canonical
// flip) and issue #139: both spellings validate against a flat input object;
// the handler enforces the at-least-one requirement before backend access.
func TestMemoryReflect_AgentIDCanonical(t *testing.T) {
	byName := listToolDefs(t)
	tool, ok := byName["memory_reflect"]
	if !ok {
		t.Fatal("memory_reflect missing")
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
			Type        string `json:"type"`
			MinLength   int    `json:"minLength"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("decode inputSchema: %v", err)
	}

	aliasProp, ok := schema.Properties["agent"]
	if !ok {
		t.Fatal("deprecated alias agent missing from memory_reflect input schema")
	}
	if !strings.Contains(strings.ToLower(aliasProp.Description), "deprecated") {
		t.Errorf("agent description %q must mark it deprecated", aliasProp.Description)
	}

	canonProp := schema.Properties["agent_id"]
	if !strings.Contains(strings.ToLower(canonProp.Description), "agent whose memory") {
		t.Errorf("agent_id description %q must state the canonical role", canonProp.Description)
	}

	if !reflect.DeepEqual(schema.Required, []string{"action"}) {
		t.Errorf("required = %v, want only action", schema.Required)
	}
	if schema.AdditionalProperties {
		t.Error("additionalProperties must remain false")
	}
	if len(schema.Properties) != 6 {
		t.Errorf("properties count = %d, want 6", len(schema.Properties))
	}
	for _, key := range []string{"action", "agent_id", "agent", "text", "target", "confidence"} {
		if schema.Properties[key].Type != "string" {
			t.Errorf("%s must retain string type", key)
		}
	}
	for _, key := range []string{"agent_id", "agent"} {
		prop := schema.Properties[key]
		if prop.MinLength != 1 {
			t.Errorf("%s.minLength = %d, want 1", key, prop.MinLength)
		}
		if !strings.Contains(prop.Description, "required at runtime") {
			t.Errorf("%s must document runtime identity requirement: %s", key, prop.Description)
		}
	}
	if !strings.Contains(tool.Description, "required at runtime") {
		t.Error("tool description must document runtime identity requirement")
	}
}

func TestMemoryReflect_InputSchemaValidatesCallerForms(t *testing.T) {
	schema := compileInputSchema(t, listToolDefs(t)["memory_reflect"].InputSchema)
	cases := []struct {
		name  string
		args  map[string]any
		valid bool
	}{
		{"canonical", map[string]any{"action": "add", "agent_id": "project"}, true},
		{"alias", map[string]any{"action": "add", "agent": "project"}, true},
		{"both", map[string]any{"action": "add", "agent_id": "canonical", "agent": "alias"}, true},
		{"no identity is runtime checked", map[string]any{"action": "add"}, true},
		{"whitespace is runtime checked", map[string]any{"action": "add", "agent_id": " \t\n"}, true},
		{"extra property", map[string]any{"action": "add", "agent_id": "project", "extra": true}, false},
		{"missing action", map[string]any{"agent_id": "project"}, false},
		{"unknown action", map[string]any{"action": "erase", "agent_id": "project"}, false},
		{"null action", map[string]any{"action": nil, "agent_id": "project"}, false},
		{"wrong text type", map[string]any{"action": "add", "agent_id": "project", "text": 7}, false},
		{"wrong target type", map[string]any{"action": "add", "agent_id": "project", "target": true}, false},
	}
	for _, key := range []string{"agent_id", "agent", "confidence"} {
		for _, value := range []any{nil, float64(7), true, []any{"project"}, map[string]any{"id": "project"}, ""} {
			args := map[string]any{"action": "add", key: value}
			cases = append(cases, struct {
				name  string
				args  map[string]any
				valid bool
			}{key + "/" + fmt.Sprint(value), args, false})
		}
	}
	for _, value := range []string{"verified", "inferred", "unverified"} {
		cases = append(cases, struct {
			name  string
			args  map[string]any
			valid bool
		}{"confidence/" + value, map[string]any{"action": "add", "confidence": value}, true})
	}
	for _, value := range []string{"unknown", " verified "} {
		cases = append(cases, struct {
			name  string
			args  map[string]any
			valid bool
		}{"confidence/" + value, map[string]any{"action": "add", "confidence": value}, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := schema.Validate(tc.args); (err == nil) != tc.valid {
				t.Errorf("schema validation = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

// TestMemoryReflect_AliasPrecedencePinned verifies the runtime rule the schema
// documents: since the flip, agent_id wins when both spellings arrive, while
// the deprecated spelling alone keeps driving every action.
func TestMemoryReflect_AliasPrecedencePinned(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// agent_id wins: the fact lands under a2's namespace, not a1's. The a1
	// probe must see the empty-state notice — asserting on the fact text alone
	// would false-positive, because the empty-state message quotes the query.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent": "a1", "agent_id": "a2", "text": "isolation probe xyzzy",
	}))
	if err != nil || res.IsError {
		t.Fatalf("reflect with both spellings failed: %v / %s", err, resultText(t, res))
	}
	for _, probe := range []struct {
		agent     string
		wantFound bool
	}{{"a2", true}, {"a1", false}} {
		res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{
			"agent_id": probe.agent, "query": "isolation probe xyzzy",
		}))
		if err != nil || res.IsError {
			t.Fatalf("search %q failed: %v / %s", probe.agent, err, resultText(t, res))
		}
		text := resultText(t, res)
		emptyState := strings.Contains(text, "No memories found")
		if probe.wantFound && (emptyState || !strings.Contains(text, "isolation probe xyzzy")) {
			t.Errorf("fact not found under %q: %s", probe.agent, text)
		}
		if !probe.wantFound && !emptyState {
			t.Errorf("agent %q must not recall a2's fact; got: %s", probe.agent, text)
		}
	}

	// The deprecated spelling alone still drives update: supersede a fact
	// stored under a3 via `agent`.
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "add", "agent_id": "a3", "text": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("seed failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update", "agent": "a3", "text": "new convention", "target": "old convention",
	}))
	if err != nil || res.IsError {
		t.Fatalf("update via deprecated alias failed: %v / %s", err, resultText(t, res))
	}
	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "a3", "query": "convention"}))
	if err != nil || res.IsError {
		t.Fatalf("search after alias update failed: %v / %s", err, resultText(t, res))
	}
	text := resultText(t, res)
	if strings.Contains(text, "old convention") || !strings.Contains(text, "new convention") {
		t.Errorf("deprecated-alias update did not supersede: %s", text)
	}
}
