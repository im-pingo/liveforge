package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionIngressUsesPublisherBoundWrites(t *testing.T) {
	moduleRoot := filepath.Join("..", "module")
	fset := token.NewFileSet()

	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "WriteFrame" || !isProductionStreamReceiver(selector.X) {
				return true
			}
			position := fset.Position(selector.Sel.Pos())
			rel, relErr := filepath.Rel("..", position.Filename)
			if relErr == nil {
				position.Filename = rel
			}
			t.Errorf("%s:%d: production stream ingress must use WriteFrameForPublisher", position.Filename, position.Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isProductionStreamReceiver(expr ast.Expr) bool {
	switch receiver := expr.(type) {
	case *ast.Ident:
		return receiver.Name == "stream"
	case *ast.SelectorExpr:
		return receiver.Sel.Name == "stream"
	default:
		return false
	}
}
