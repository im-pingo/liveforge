package cluster

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionReaderGuardRejectsRingHistory(t *testing.T) {
	var violations []token.Position
	moduleRoot := filepath.Join("..")
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := ringHistoryReaders(path, src)
		if err != nil {
			return err
		}
		violations = append(violations, found...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range violations {
		t.Errorf("%s:%d: production egress must use a snapshot-bound RingReader", position.Filename, position.Line)
	}
}

func TestProductionReaderGuardAllowsLocalBuffers(t *testing.T) {
	violations, err := ringHistoryReaders("fixture.go", []byte(`package fixture
func local() {
	localBuffer := newBuffer()
	localBuffer.NewReader()
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("local buffer violations = %v, want none", violations)
	}
}

func ringHistoryReaders(filename string, src []byte) ([]token.Position, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var violations []token.Position
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != "NewReader" {
			return true
		}
		inner, ok := outer.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RingBuffer" {
			return true
		}
		violations = append(violations, fset.Position(outer.Sel.Pos()))
		return true
	})
	return violations, nil
}
