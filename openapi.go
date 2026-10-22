// Package backend holds the API's own description, openapi.yaml, built into the binary so the
// published spec always matches the API that's running (TL-484). The service lives in cmd/ and internal/.
package backend

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"go.yaml.in/yaml/v3"
)

//go:embed openapi.yaml
var OpenAPIYAML []byte

// OpenAPIJSON is the spec as JSON, for the website's API reference.
func OpenAPIJSON() ([]byte, error) {
	var spec any
	if err := yaml.Unmarshal(OpenAPIYAML, &spec); err != nil {
		return nil, fmt.Errorf("openapi.yaml: %w", err)
	}
	return json.Marshal(spec)
}
