package extract

import (
	"encoding/json"
	"reflect"
	"testing"
)

// walkObjects visits every JSON-schema object node, naming its path so a
// failure says which one is wrong rather than that something is.
func walkObjects(t *testing.T, path string, node map[string]any, visit func(string, map[string]any)) {
	t.Helper()
	if node["type"] == "object" {
		visit(path, node)
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, child := range props {
			if m, ok := child.(map[string]any); ok {
				walkObjects(t, path+"."+name, m, visit)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkObjects(t, path+"[]", items, visit)
	}
}

// Newer OpenAI models validate a response_format schema whether or not
// `strict` is set, and reject one that leaves any object open or any
// property untyped. gpt-4o-mini accepts either shape, so the compliant one
// is simply the shape that works everywhere. Verified against the live API
// on 2026-09-22: as shipped before this, gpt-5-nano and gpt-4.1-nano failed
// every single extraction with a 400.
func TestOperationSchemasAreAcceptedByStrictValidators(t *testing.T) {
	schemas := map[string]json.RawMessage{
		"operationSchema":                operationSchema,
		"operationSchemaWithCommitments": operationSchemaWithCommitments,
	}

	for name, raw := range schemas {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(raw, &root); err != nil {
				t.Fatalf("schema is not valid JSON: %v", err)
			}

			walkObjects(t, name, root, func(path string, node map[string]any) {
				if node["additionalProperties"] != false {
					t.Errorf("%s: additionalProperties must be present and false", path)
				}

				props, _ := node["properties"].(map[string]any)
				required := map[string]bool{}
				for _, r := range node["required"].([]any) {
					required[r.(string)] = true
				}
				for prop, def := range props {
					if !required[prop] {
						t.Errorf("%s.%s: every property must be listed in required", path, prop)
					}
					if m, ok := def.(map[string]any); !ok || m["type"] == nil {
						t.Errorf("%s.%s: every property needs a type", path, prop)
					}
				}
			})
		})
	}
}

// The schema now asks for `value` as a JSON-encoded string, because strict
// validation cannot express free-form JSON. Nothing may be lost by that:
// 9% of the facts in a real store are objects or arrays, and they have to
// survive the round trip. The raw forms stay handled too, because the
// Anthropic path ignores the schema entirely and sends what the prompt
// asked for.
func TestValueToMapAcceptsEncodedAndRawValues(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{"encoded object", `"{\"host\":\"db1\",\"port\":5432}"`, map[string]any{"host": "db1", "port": float64(5432)}},
		{"encoded array", `"[1,2]"`, map[string]any{"items": []any{float64(1), float64(2)}}},
		{"encoded number", `"42"`, map[string]any{"v": float64(42)}},
		{"encoded boolean", `"true"`, map[string]any{"v": true}},
		{"plain prose stays a string", `"ships on Friday"`, map[string]any{"v": "ships on Friday"}},
		{"raw object still works", `{"a":1}`, map[string]any{"a": float64(1)}},
		{"raw array still works", `[1,2]`, map[string]any{"items": []any{float64(1), float64(2)}}},
		{"raw number still works", `42`, map[string]any{"v": float64(42)}},
		{"null is empty", `null`, map[string]any{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := valueToMap(json.RawMessage(c.raw)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("valueToMap(%s) = %#v, want %#v", c.raw, got, c.want)
			}
		})
	}
}
