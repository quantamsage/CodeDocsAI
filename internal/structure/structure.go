// Package structure extracts structural facts from source files without
// executing them: packages, imports, functions, methods, types, constants,
// variables, HTTP route registrations, SQL tables and declared dependencies.
//
// Go is parsed with go/parser and go/ast, so its facts are exact. Other
// languages use per-language line patterns, which find the common forms of
// declarations but can miss unusual ones; their results are marked
// "heuristic" so consumers know how far to trust them. The facts ground the
// AI analysis: they tell the model what exists, and they are used to reject
// AI claims about functions that are not in the file.
package structure

import (
	"path"
	"strings"
)

// Parser names reported in FileStructure.Parser.
const (
	ParserGoAST     = "go/ast"
	ParserHeuristic = "heuristic"
	ParserManifest  = "manifest"
	ParserNone      = "none"
)

// Symbol kinds.
const (
	KindFunction  = "function"
	KindMethod    = "method"
	KindStruct    = "struct"
	KindInterface = "interface"
	KindType      = "type"
	KindClass     = "class"
	KindModule    = "module"
	KindConst     = "const"
	KindVar       = "var"
	KindTable     = "table"
)

// MaxSymbols bounds the symbols recorded per file.
const MaxSymbols = 300

// Symbol is one declaration.
type Symbol struct {
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Receiver  string   `json:"receiver,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Exported  bool     `json:"exported"`
	Line      int      `json:"line"`
	Doc       string   `json:"doc,omitempty"`
	Members   []string `json:"members,omitempty"` // struct fields, interface methods
}

// Route is an HTTP route registration found in code.
type Route struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

// FileStructure is everything extracted from one file.
type FileStructure struct {
	Path         string   `json:"path"`
	Language     string   `json:"language"`
	Parser       string   `json:"parser"`
	Lines        int      `json:"lines"`
	Package      string   `json:"package,omitempty"`
	Imports      []string `json:"imports,omitempty"`
	Symbols      []Symbol `json:"symbols,omitempty"`
	Routes       []Route  `json:"routes,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	ParseError   string   `json:"parse_error,omitempty"`
}

// Extract returns the structure of a file. It never fails: unparseable
// content yields a structure with ParseError set and whatever was found.
func Extract(filePath, language string, content []byte) FileStructure {
	text := string(content)
	fs := FileStructure{Path: filePath, Language: language, Parser: ParserNone, Lines: countLines(text)}

	if deps, ok := manifestDependencies(filePath, text); ok {
		fs.Parser = ParserManifest
		fs.Dependencies = deps
		return fs
	}

	switch language {
	case "Go":
		extractGo(&fs, content)
	case "SQL":
		extractSQL(&fs, text)
	default:
		if spec, ok := languageSpecs[language]; ok {
			extractWithSpec(&fs, spec, text)
		}
	}
	fs.Routes = append(fs.Routes, findRoutes(language, text)...)
	if len(fs.Symbols) > MaxSymbols {
		fs.Symbols = fs.Symbols[:MaxSymbols]
	}
	return fs
}

// Names returns the set of symbol names (and receiver-qualified method
// names) declared in the file.
func (fs FileStructure) Names() map[string]bool {
	names := make(map[string]bool, len(fs.Symbols)*2)
	for _, s := range fs.Symbols {
		names[s.Name] = true
		if s.Receiver != "" {
			names[strings.TrimPrefix(s.Receiver, "*")+"."+s.Name] = true
		}
	}
	return names
}

// Summary renders the structure compactly for an AI prompt.
func (fs FileStructure) Summary() string {
	var b strings.Builder
	b.WriteString("parser: " + fs.Parser)
	if fs.Package != "" {
		b.WriteString("\npackage: " + fs.Package)
	}
	if len(fs.Imports) > 0 {
		b.WriteString("\nimports: " + strings.Join(limit(fs.Imports, 40), ", "))
	}
	for _, s := range limitSymbols(fs.Symbols, 150) {
		b.WriteString("\n- " + s.Kind + " ")
		if s.Signature != "" {
			b.WriteString(s.Signature)
		} else {
			b.WriteString(s.Name)
		}
		if len(s.Members) > 0 {
			b.WriteString(" {" + strings.Join(limit(s.Members, 20), "; ") + "}")
		}
	}
	for _, r := range fs.Routes {
		b.WriteString("\n- route " + strings.TrimSpace(r.Method+" "+r.Path))
	}
	if len(fs.Dependencies) > 0 {
		b.WriteString("\ndependencies: " + strings.Join(limit(fs.Dependencies, 60), ", "))
	}
	if fs.ParseError != "" {
		b.WriteString("\nparse error: " + fs.ParseError)
	}
	return b.String()
}

func limit(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(append([]string(nil), items[:n]...), "…")
}

func limitSymbols(items []Symbol, n int) []Symbol {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

func base(p string) string { return strings.ToLower(path.Base(p)) }
