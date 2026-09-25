package management

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
)

// TestAuthFileNameSafetyParity keeps the fork's authfiles.IsUnsafeAuthFileName
// identical to the upstream management isUnsafeAuthFileName.
func TestAuthFileNameSafetyParity(t *testing.T) {
	names := []string{
		"", " ", "\t", "a.json", "A.JSON", "../a.json", "a/b.json", `a\b.json`,
		"C:a.json", `C:\a.json`, `\\server\share\a.json`, ".", "..", "a b.json",
		" a.json ", "a.json\x00", "日本.json",
	}
	for _, name := range names {
		if got, want := authfiles.IsUnsafeAuthFileName(name), isUnsafeAuthFileName(name); got != want {
			t.Errorf("IsUnsafeAuthFileName(%q) = %v, upstream = %v", name, got, want)
		}
	}
}

// TestUserExposedOAuthFlowsBindSessionToUser guards the tenant OAuth seam: every
// provider flow reachable from the /v0/user API must register its OAuth state
// through registerOAuthSessionForRequest, which binds the session to the
// requesting tenant so another tenant cannot complete or observe it.
func TestUserExposedOAuthFlowsBindSessionToUser(t *testing.T) {
	flows := map[string]string{
		"RequestAnthropicToken":      "",
		"RequestCodexToken":          "",
		"RequestAntigravityToken":    "",
		"requestKimiTokenWithDomain": "", // RequestKimiToken delegates here.
		"RequestXAIToken":            "",
	}
	fileSet := token.NewFileSet()
	file, errParse := parser.ParseFile(fileSet, "auth_files_provider_oauth.go", nil, 0)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	found := make(map[string]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, tracked := flows[fn.Name.Name]; !tracked {
			continue
		}
		bound, unbound := false, false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				switch ident.Name {
				case "registerOAuthSessionForRequest":
					bound = true
				case "RegisterOAuthSession":
					unbound = true
				}
			}
			return true
		})
		found[fn.Name.Name] = true
		if !bound || unbound {
			t.Errorf("%s: bound=%v unbound=%v; tenant OAuth sessions must use registerOAuthSessionForRequest", fn.Name.Name, bound, unbound)
		}
	}
	for name := range flows {
		if !found[name] {
			t.Errorf("%s not found in auth_files_provider_oauth.go; update this guard and the fork ledger", name)
		}
	}
}
