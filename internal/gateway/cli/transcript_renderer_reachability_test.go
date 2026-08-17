package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTranscriptRenderersHaveProductionReferences closes the failure mode that
// left renderExploreGroup unit-tested but unreachable from the real TUI. The
// check intentionally scans non-test package source only: a direct renderer
// unit test is useful for formatting, but it is not evidence of production
// message-path integration.
func TestTranscriptRenderersHaveProductionReferences(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files[name] = parsed
	}
	target := files["transcript_renderer.go"]
	if target == nil {
		t.Fatal("transcript_renderer.go was not parsed")
	}
	for _, decl := range target.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		uses := 0
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				ident, isIdent := node.(*ast.Ident)
				if isIdent && ident.Name == fn.Name.Name && ident.Pos() != fn.Name.Pos() {
					uses++
				}
				return true
			})
		}
		if uses == 0 {
			t.Errorf("%s has no production reference; wire it through the real message path or remove it", fn.Name.Name)
		}
	}
}
