package backend

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Every httpx.Code in internal/ is in openapi.yaml's x-error-codes and the other way round, so a code
// isn't added undocumented or renamed/removed once shipped (TL-552). A code keeps one message, a
// message one code, and the documented message is the code's English (dynamic ones have a template).
func TestErrorCodesDocumented(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas struct {
				ErrorResponse struct {
					Codes map[string]string `yaml:"x-error-codes"`
				} `yaml:"ErrorResponse"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(OpenAPIYAML, &spec); err != nil {
		t.Fatal(err)
	}
	documented := spec.Components.Schemas.ErrorResponse.Codes

	msgs := map[string]string{}  // code → literal message
	codes := map[string]string{} // literal message → code
	seen := map[string]string{}  // code → where
	err := filepath.WalkDir("internal", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 || !isCode(call.Fun, f.Name.Name) {
				return true
			}
			code, ok := str(call.Args[0])
			if !ok {
				t.Errorf("%s: httpx.Code needs a literal code", path)
				return true
			}
			seen[code] = path
			msg, ok := str(call.Args[1])
			if !ok {
				return true // built at run time: the doc has its template
			}
			if prev, ok := msgs[code]; ok && prev != msg {
				t.Errorf("%s: code %s has two messages: %q and %q", path, code, prev, msg)
			}
			if prev, ok := codes[msg]; ok && prev != code {
				t.Errorf("%s: %q has two codes: %s and %s", path, msg, prev, code)
			}
			msgs[code], codes[msg] = msg, code
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 100 {
		t.Fatalf("only %d codes found: is the scan still looking at internal/?", len(seen))
	}
	for code, path := range seen {
		doc, ok := documented[code]
		switch {
		case !ok:
			t.Errorf("%s: code %s isn't in openapi.yaml's x-error-codes", path, code)
		case msgs[code] != "" && doc != msgs[code]:
			t.Errorf("x-error-codes %s: documented %q, the code says %q", code, doc, msgs[code])
		}
	}
	for code := range documented {
		if _, ok := seen[code]; !ok {
			t.Errorf("x-error-codes lists %s, which no code uses: shipped codes stay", code)
		}
	}
}

// isCode: httpx.Code(…), or Code(…) inside httpx.
func isCode(fun ast.Expr, pkg string) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "httpx" && f.Sel.Name == "Code"
	case *ast.Ident:
		return pkg == "httpx" && f.Name == "Code"
	}
	return false
}

func str(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
