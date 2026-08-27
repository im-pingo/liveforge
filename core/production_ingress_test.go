package core

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

func TestProductionIngressUsesPublisherBoundWrites(t *testing.T) {
	moduleRoot := filepath.Join("..", "module")
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

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		violations, parseErr := productionIngressViolations(path, src)
		if parseErr != nil {
			return parseErr
		}
		for _, position := range violations {
			rel, relErr := filepath.Rel("..", position.Filename)
			if relErr == nil {
				position.Filename = rel
			}
			t.Errorf("%s:%d: production stream ingress must use WriteFrameForPublisher", position.Filename, position.Line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProductionIngressGuardRejectsKnownFieldsAndAliases(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "exported stream field",
			src: `package fixture
func ingest(h *handler, frame *Frame) {
	h.Stream.WriteFrame(frame)
}`,
		},
		{
			name: "local alias",
			src: `package fixture
func ingest(stream *Stream, frame *Frame) {
	s := stream
	s.WriteFrame(frame)
}`,
		},
		{
			name: "chained field alias",
			src: `package fixture
func ingest(h *handler, frame *Frame) {
	s := h.Stream
	alias := s
	alias.WriteFrame(frame)
}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, err := productionIngressViolations("fixture.go", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 1 {
				t.Fatalf("violations = %v, want one raw stream write", violations)
			}
		})
	}
}

func TestProductionIngressGuardAllowsMuxerAndFileWriterWrites(t *testing.T) {
	src := `package fixture
func output(muxer *Muxer, writer *FileWriter, frame *Frame) {
	muxer.WriteFrame(frame)
	writer.WriteFrame(frame)
}`
	violations, err := productionIngressViolations("fixture.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("non-ingress violations = %v, want none", violations)
	}
}

func productionIngressViolations(filename string, src []byte) ([]token.Position, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var violations []token.Position
	ast.Inspect(file, func(node ast.Node) bool {
		var body *ast.BlockStmt
		switch fn := node.(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		default:
			return true
		}
		if body == nil {
			return false
		}
		aliases := productionStreamAliases(body)
		ast.Inspect(body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "WriteFrame" || !isProductionStreamReceiver(selector.X, aliases) {
				return true
			}
			violations = append(violations, fset.Position(selector.Sel.Pos()))
			return true
		})
		return false
	})
	return violations, nil
}

func productionStreamAliases(body *ast.BlockStmt) map[*ast.Object]bool {
	aliases := make(map[*ast.Object]bool)
	changed := true
	for changed {
		changed = false
		ast.Inspect(body, func(node ast.Node) bool {
			switch stmt := node.(type) {
			case *ast.AssignStmt:
				for i, lhs := range stmt.Lhs {
					if i >= len(stmt.Rhs) || !isProductionStreamReceiver(stmt.Rhs[i], aliases) {
						continue
					}
					ident, ok := lhs.(*ast.Ident)
					if ok && ident.Obj != nil && !aliases[ident.Obj] {
						aliases[ident.Obj] = true
						changed = true
					}
				}
			case *ast.ValueSpec:
				for i, name := range stmt.Names {
					if i >= len(stmt.Values) || !isProductionStreamReceiver(stmt.Values[i], aliases) {
						continue
					}
					if name.Obj != nil && !aliases[name.Obj] {
						aliases[name.Obj] = true
						changed = true
					}
				}
			}
			return true
		})
	}
	return aliases
}

func isProductionStreamReceiver(expr ast.Expr, aliases map[*ast.Object]bool) bool {
	switch receiver := expr.(type) {
	case *ast.Ident:
		return receiver.Name == "stream" || receiver.Obj != nil && aliases[receiver.Obj]
	case *ast.SelectorExpr:
		return receiver.Sel.Name == "stream" || receiver.Sel.Name == "Stream"
	case *ast.ParenExpr:
		return isProductionStreamReceiver(receiver.X, aliases)
	default:
		return false
	}
}
