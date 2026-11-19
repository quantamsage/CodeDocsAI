package structure

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// extractGo parses Go source with go/parser. Parsing never executes code.
func extractGo(fs *FileStructure, content []byte) {
	fs.Parser = ParserGoAST
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fs.Path, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		fs.ParseError = firstLine(err.Error())
		if file == nil {
			return
		}
	}
	fs.Package = file.Name.Name
	for _, imp := range file.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			fs.Imports = append(fs.Imports, p)
		}
	}
	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := Symbol{
				Kind:     KindFunction,
				Name:     d.Name.Name,
				Exported: d.Name.IsExported(),
				Line:     line(d.Pos()),
				Doc:      docLine(d.Doc),
			}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = KindMethod
				sym.Receiver = types.ExprString(d.Recv.List[0].Type)
			}
			sym.Signature = funcSignature(d)
			fs.Symbols = append(fs.Symbols, sym)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					fs.Symbols = append(fs.Symbols, typeSymbol(s, d, line(s.Pos())))
				case *ast.ValueSpec:
					kind := KindVar
					if d.Tok == token.CONST {
						kind = KindConst
					}
					for _, name := range s.Names {
						if name.Name == "_" {
							continue
						}
						sym := Symbol{Kind: kind, Name: name.Name, Exported: name.IsExported(), Line: line(name.Pos()), Doc: docLine(s.Doc)}
						if sym.Doc == "" && len(d.Specs) == 1 {
							sym.Doc = docLine(d.Doc)
						}
						if s.Type != nil {
							sym.Signature = name.Name + " " + types.ExprString(s.Type)
						}
						fs.Symbols = append(fs.Symbols, sym)
					}
				}
			}
		}
	}
}

func typeSymbol(s *ast.TypeSpec, d *ast.GenDecl, line int) Symbol {
	sym := Symbol{Kind: KindType, Name: s.Name.Name, Exported: s.Name.IsExported(), Line: line, Doc: docLine(s.Doc)}
	if sym.Doc == "" && len(d.Specs) == 1 {
		sym.Doc = docLine(d.Doc)
	}
	switch t := s.Type.(type) {
	case *ast.StructType:
		sym.Kind = KindStruct
		for _, field := range t.Fields.List {
			typ := types.ExprString(field.Type)
			tag := ""
			if field.Tag != nil {
				tag = " " + field.Tag.Value
			}
			if len(field.Names) == 0 {
				sym.Members = append(sym.Members, typ+tag) // embedded
			}
			for _, name := range field.Names {
				sym.Members = append(sym.Members, name.Name+" "+typ+tag)
			}
		}
	case *ast.InterfaceType:
		sym.Kind = KindInterface
		for _, m := range t.Methods.List {
			if len(m.Names) == 0 {
				sym.Members = append(sym.Members, types.ExprString(m.Type))
				continue
			}
			if ft, ok := m.Type.(*ast.FuncType); ok {
				sym.Members = append(sym.Members, m.Names[0].Name+strings.TrimPrefix(types.ExprString(ft), "func"))
			}
		}
	default:
		sym.Signature = "type " + s.Name.Name + " " + types.ExprString(s.Type)
	}
	return sym
}

func funcSignature(d *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		recv := d.Recv.List[0]
		b.WriteString("(")
		if len(recv.Names) > 0 {
			b.WriteString(recv.Names[0].Name + " ")
		}
		b.WriteString(types.ExprString(recv.Type) + ") ")
	}
	b.WriteString(d.Name.Name)
	b.WriteString(strings.TrimPrefix(types.ExprString(d.Type), "func"))
	return b.String()
}

func docLine(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	text := strings.TrimSpace(group.Text())
	if i := strings.Index(text, "\n\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 240 {
		text = text[:237] + "…"
	}
	return text
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
