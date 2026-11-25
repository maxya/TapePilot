package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// Validate the flat Archive response against the same external JSON Schema
// referenced by OpenAPI, including required keys and the legacy null value.
func verifyArchiveResponse(t *testing.T, raw []byte) {
	t.Helper()
	data, e := os.ReadFile("../../schemas/archive-response.json")
	if e != nil {
		t.Fatal(e)
	}
	var schema struct {
		Properties           map[string]struct{ Type any }
		Required             []string
		AdditionalProperties bool
	}
	if e = json.Unmarshal(data, &schema); e != nil {
		t.Fatal(e)
	}
	var rows []map[string]any
	if e = json.Unmarshal(raw, &rows); e != nil {
		t.Fatal(e)
	}
	kinds := map[string]bool{}
	for _, row := range rows {
		for _, key := range schema.Required {
			if _, ok := row[key]; !ok {
				t.Fatalf("missing %s", key)
			}
		}
		for key, v := range row {
			p, ok := schema.Properties[key]
			if !ok && !schema.AdditionalProperties {
				t.Fatal("undocumented field", key)
			}
			typ := "null"
			switch x := v.(type) {
			case string:
				typ = "string"
			case bool:
				typ = "boolean"
			case float64:
				if x != float64(int64(x)) {
					t.Fatal(key, x)
				}
				typ = "integer"
			case nil:
			default:
				t.Fatal("unexpected field type", key)
			}
			switch x := p.Type.(type) {
			case string:
				if typ != x {
					t.Fatal(key, x, typ)
				}
			case []any:
				if !slices.Contains(x, any(typ)) {
					t.Fatal(key, x, typ)
				}
			default:
				t.Fatal(key, "schema lacks type")
			}
			if key == "commit_file_index" {
				kinds[typ] = true
			}
		}
	}
	if !kinds["null"] || !kinds["integer"] {
		t.Fatal("required managed and legacy HTTP rows", kinds)
	}
}
