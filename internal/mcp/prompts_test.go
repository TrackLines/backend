package mcp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TL-565: the docs get exactly what agents are told, every tool included.
func TestPrompts(t *testing.T) {
	rec := httptest.NewRecorder()
	NewPromptStore(nil).Prompts(rec, httptest.NewRequest("GET", "/api/mcp/prompts", nil)) // no database: the built-in text
	var got struct {
		Instructions string   `json:"instructions"`
		Tools        []Prompt `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Instructions != Instructions || len(got.Tools) != len(tools) {
		t.Fatalf("instructions match: %v, tools %d of %d", got.Instructions == Instructions, len(got.Tools), len(tools))
	}
	for i, p := range got.Tools {
		if p.Name != tools[i].name || p.Description != tools[i].description || p.Description == "" {
			t.Errorf("tool %d: %+v", i, p)
		}
	}
}
