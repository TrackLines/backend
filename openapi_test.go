package backend

import (
	"encoding/json"
	"testing"
)

// The embedded spec parses, and turns into JSON the website can read: an OpenAPI 3 document with paths.
func TestOpenAPI(t *testing.T) {
	raw, err := OpenAPIJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI[:2] != "3." || len(spec.Paths) < 50 || spec.Paths["/keys"]["get"] == nil {
		t.Fatalf("spec: openapi %q, %d paths", spec.OpenAPI, len(spec.Paths))
	}
}
