package structure

import (
	"regexp"
	"strings"
)

// symbolPattern finds one kind of declaration. The `name` group is the
// declared name; optional `sig` captures parameters; optional `indent`
// distinguishes methods (indented) from functions.
type symbolPattern struct {
	kind string
	re   *regexp.Regexp
	// nestedKind, if set, is used when the declaration is indented.
	nestedKind string
}

type languageSpec struct {
	imports  []*regexp.Regexp // first non-empty submatch is the import
	symbols  []symbolPattern
	exported func(name, line string) bool
	pkg      *regexp.Regexp
}

var (
	reIdent = `[A-Za-z_$][\w$]*`

	languageSpecs = map[string]languageSpec{
		"Python": {
			imports: []*regexp.Regexp{
				regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\b`),
				regexp.MustCompile(`^\s*import\s+([\w.]+)`),
			},
			symbols: []symbolPattern{
				{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)class\s+(?P<name>\w+)\s*(?P<sig>\([^)]*\))?\s*:`)},
				{kind: KindFunction, nestedKind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s*)(?:async\s+)?def\s+(?P<name>\w+)\s*(?P<sig>\([^)]*\)?)`)},
			},
			exported: func(name, _ string) bool { return !strings.HasPrefix(name, "_") },
		},
		"JavaScript": jsSpec,
		"TypeScript": jsSpec,
		"Java":       jvmSpec("import"),
		"Kotlin": {
			imports: []*regexp.Regexp{regexp.MustCompile(`^\s*import\s+([\w.*]+)`)},
			symbols: []symbolPattern{
				{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)(?:[\w]+\s+)*(?:class|interface|object)\s+(?P<name>\w+)`)},
				{kind: KindFunction, nestedKind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s*)(?:[\w]+\s+)*fun\s+(?:<[^>]+>\s*)?(?:[\w.]+\.)?(?P<name>\w+)\s*(?P<sig>\([^)]*\)?)`)},
			},
			exported: func(_, line string) bool { return !strings.Contains(line, "private ") },
			pkg:      regexp.MustCompile(`^\s*package\s+([\w.]+)`),
		},
		"C#": jvmSpec("using"),
		"Rust": {
			imports: []*regexp.Regexp{regexp.MustCompile(`^\s*(?:pub\s+)?use\s+([\w:]+)`), regexp.MustCompile(`^\s*extern\s+crate\s+(\w+)`)},
			symbols: []symbolPattern{
				{kind: KindStruct, re: regexp.MustCompile(`^(?P<indent>\s*)(?:pub(?:\([^)]*\))?\s+)?struct\s+(?P<name>\w+)`)},
				{kind: KindType, re: regexp.MustCompile(`^(?P<indent>\s*)(?:pub(?:\([^)]*\))?\s+)?(?:enum|type)\s+(?P<name>\w+)`)},
				{kind: KindInterface, re: regexp.MustCompile(`^(?P<indent>\s*)(?:pub(?:\([^)]*\))?\s+)?(?:unsafe\s+)?trait\s+(?P<name>\w+)`)},
				{kind: KindFunction, nestedKind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s*)(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+(?P<name>\w+)\s*(?:<[^>]*>)?\s*(?P<sig>\([^)]*\)?)`)},
				{kind: KindConst, re: regexp.MustCompile(`^(?P<indent>)(?:pub(?:\([^)]*\))?\s+)?(?:const|static)\s+(?P<name>[A-Z_][A-Z0-9_]*)\s*:`)},
			},
			exported: func(_, line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "pub") },
		},
		"PHP": {
			imports: []*regexp.Regexp{regexp.MustCompile(`^\s*use\s+([\w\\]+)`), regexp.MustCompile(`^\s*(?:require|include)(?:_once)?\s*\(?\s*['"]([^'"]+)['"]`)},
			symbols: []symbolPattern{
				{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)(?:abstract\s+|final\s+)?(?:class|trait|enum)\s+(?P<name>\w+)`)},
				{kind: KindInterface, re: regexp.MustCompile(`^(?P<indent>\s*)interface\s+(?P<name>\w+)`)},
				{kind: KindFunction, nestedKind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s*)(?:(?:public|private|protected|static|abstract|final)\s+)*function\s+&?(?P<name>\w+)\s*(?P<sig>\([^)]*\)?)`)},
			},
			exported: func(_, line string) bool { return !strings.Contains(line, "private ") },
			pkg:      regexp.MustCompile(`^\s*namespace\s+([\w\\]+)`),
		},
		"Ruby": {
			imports: []*regexp.Regexp{regexp.MustCompile(`^\s*require(?:_relative)?\s*\(?\s*['"]([^'"]+)['"]`)},
			symbols: []symbolPattern{
				{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)class\s+(?P<name>[\w:]+)`)},
				{kind: KindModule, re: regexp.MustCompile(`^(?P<indent>\s*)module\s+(?P<name>[\w:]+)`)},
				{kind: KindFunction, nestedKind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s*)def\s+(?:self\.)?(?P<name>[\w]+[?!=]?)\s*(?P<sig>\([^)]*\)?)?`)},
			},
			exported: func(name, _ string) bool { return !strings.HasPrefix(name, "_") },
		},
		"C":   cSpec,
		"C++": cSpec,
	}

	jsSpec = languageSpec{
		imports: []*regexp.Regexp{
			regexp.MustCompile(`^\s*import\s+(?:[^'"]*\s+from\s+)?['"]([^'"]+)['"]`),
			regexp.MustCompile(`^\s*export\s+[^'"]*\s+from\s+['"]([^'"]+)['"]`),
			regexp.MustCompile(`\brequire\(\s*['"]([^'"]+)['"]\s*\)`),
		},
		symbols: []symbolPattern{
			{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+(?P<name>` + reIdent + `)`)},
			{kind: KindInterface, re: regexp.MustCompile(`^(?P<indent>\s*)(?:export\s+)?interface\s+(?P<name>` + reIdent + `)`)},
			{kind: KindType, re: regexp.MustCompile(`^(?P<indent>\s*)(?:export\s+)?(?:type|enum)\s+(?P<name>` + reIdent + `)\s*[=<{]`)},
			{kind: KindFunction, re: regexp.MustCompile(`^(?P<indent>\s*)(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*(?P<name>` + reIdent + `)\s*(?:<[^>]*>)?\s*(?P<sig>\([^)]*\)?)`)},
			{kind: KindFunction, re: regexp.MustCompile(`^(?P<indent>)(?:export\s+)?(?:const|let|var)\s+(?P<name>` + reIdent + `)\s*(?::[^=]+)?=\s*(?:async\s+)?(?P<sig>\([^)]*\)|` + reIdent + `)\s*(?::[^=]+)?=>`)},
			{kind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s+)(?:public\s+|private\s+|protected\s+|static\s+|async\s+|get\s+|set\s+)*(?P<name>` + reIdent + `)\s*(?P<sig>\([^)]*\))\s*(?::\s*[^{]+)?\{\s*$`)},
		},
		exported: func(_, line string) bool {
			return strings.Contains(line, "export ") || strings.HasPrefix(strings.TrimSpace(line), "module.exports")
		},
	}

	cSpec = languageSpec{
		imports: []*regexp.Regexp{regexp.MustCompile(`^\s*#\s*include\s*[<"]([^>"]+)[>"]`)},
		symbols: []symbolPattern{
			{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)(?:template\s*<[^>]*>\s*)?class\s+(?P<name>\w+)\s*(?:final\s*)?[:{]`)},
			{kind: KindStruct, re: regexp.MustCompile(`^(?P<indent>\s*)(?:typedef\s+)?struct\s+(?P<name>\w+)\s*\{`)},
			{kind: KindFunction, re: regexp.MustCompile(`^(?P<indent>)(?:static\s+|inline\s+|extern\s+|virtual\s+|constexpr\s+)*[\w:<>,*&\s]+?[\s*&]+(?P<name>[A-Za-z_][\w:~]*)\s*(?P<sig>\([^;{)]*\))\s*(?:const\s*)?(?:noexcept\s*)?\{?\s*$`)},
		},
		exported: func(_, line string) bool { return !strings.HasPrefix(strings.TrimSpace(line), "static ") },
	}
)

func jvmSpec(importKeyword string) languageSpec {
	return languageSpec{
		imports: []*regexp.Regexp{regexp.MustCompile(`^\s*` + importKeyword + `\s+(?:static\s+)?([\w.*]+)\s*;`)},
		symbols: []symbolPattern{
			{kind: KindInterface, re: regexp.MustCompile(`^(?P<indent>\s*)(?:(?:public|private|protected|internal|static|sealed|partial|abstract)\s+)*interface\s+(?P<name>\w+)`)},
			{kind: KindClass, re: regexp.MustCompile(`^(?P<indent>\s*)(?:(?:public|private|protected|internal|static|sealed|partial|abstract|final)\s+)*(?:class|enum|record|struct)\s+(?P<name>\w+)`)},
			{kind: KindMethod, re: regexp.MustCompile(`^(?P<indent>\s+)(?:(?:public|private|protected|internal|static|final|override|virtual|abstract|async|synchronized|sealed)\s+)+[\w<>\[\],.?\s]*?\s(?P<name>\w+)\s*(?P<sig>\([^)]*\)?)\s*(?:throws\s+[\w.,\s]+)?\s*(?:\{|=>|$)`)},
		},
		exported: func(_, line string) bool { return strings.Contains(line, "public ") },
		pkg:      regexp.MustCompile(`^\s*(?:package|namespace)\s+([\w.]+)`),
	}
}

var controlWords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true, "else": true,
	"sizeof": true, "do": true, "case": true, "elif": true, "unless": true, "until": true,
}

func extractWithSpec(fs *FileStructure, spec languageSpec, text string) {
	fs.Parser = ParserHeuristic
	seenImport := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		lineNo := i + 1
		line = strings.TrimRight(line, "\r")
		if spec.pkg != nil && fs.Package == "" {
			if m := spec.pkg.FindStringSubmatch(line); m != nil {
				fs.Package = m[1]
			}
		}
		for _, re := range spec.imports {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				if imp := m[1]; imp != "" && !seenImport[imp] {
					seenImport[imp] = true
					fs.Imports = append(fs.Imports, imp)
				}
			}
		}
		for _, p := range spec.symbols {
			m := p.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[p.re.SubexpIndex("name")]
			if name == "" || controlWords[name] {
				continue
			}
			kind := p.kind
			indent := ""
			if idx := p.re.SubexpIndex("indent"); idx >= 0 {
				indent = m[idx]
			}
			if p.nestedKind != "" && indent != "" {
				kind = p.nestedKind
			}
			sym := Symbol{Kind: kind, Name: name, Line: lineNo, Exported: spec.exported(name, line)}
			if idx := p.re.SubexpIndex("sig"); idx >= 0 && m[idx] != "" {
				sym.Signature = name + strings.Join(strings.Fields(m[idx]), " ")
			}
			fs.Symbols = append(fs.Symbols, sym)
			break // one declaration per line
		}
	}
}

var reCreateTable = regexp.MustCompile(`(?i)\bcreate\s+(?:virtual\s+)?table\s+(?:if\s+not\s+exists\s+)?["\x60\[]?(\w+)`)

func extractSQL(fs *FileStructure, text string) {
	fs.Parser = ParserHeuristic
	for i, line := range strings.Split(text, "\n") {
		if m := reCreateTable.FindStringSubmatch(line); m != nil {
			fs.Symbols = append(fs.Symbols, Symbol{Kind: KindTable, Name: m[1], Line: i + 1, Exported: true})
		}
	}
}

var (
	reGoRoute     = regexp.MustCompile(`\.(?:Handle|HandleFunc|GET|POST|PUT|DELETE|PATCH|Get|Post|Put|Delete|Patch|Any)\(\s*"([^"]+)"`)
	reJSRoute     = regexp.MustCompile("\\b(?:app|router|server|api|routes)\\.(get|post|put|delete|patch|all)\\(\\s*['\"`](/[^'\"`]*)")
	rePyRoute     = regexp.MustCompile(`@\w+\.(route|get|post|put|delete|patch|api_route)\(\s*['"](/[^'"]*)`)
	reSpringRoute = regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Request)Mapping\(\s*(?:(?:value|path)\s*=\s*)?"([^"]*)"`)
	reASPRoute    = regexp.MustCompile(`\[(?:Http(Get|Post|Put|Delete|Patch)|Route)\(\s*"([^"]*)"`)
	rePHPRoute    = regexp.MustCompile(`Route::(get|post|put|delete|patch|any)\(\s*['"]([^'"]+)`)
	reRubyRoute   = regexp.MustCompile(`^\s*(get|post|put|patch|delete)\s+['"]([^'"]+)['"]`)
	reMethodPath  = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(\S+)$`)
)

// findRoutes finds HTTP route registrations in common frameworks.
func findRoutes(language, text string) []Route {
	var res []*regexp.Regexp
	switch language {
	case "Go":
		res = []*regexp.Regexp{reGoRoute}
	case "JavaScript", "TypeScript":
		res = []*regexp.Regexp{reJSRoute}
	case "Python":
		res = []*regexp.Regexp{rePyRoute}
	case "Java", "Kotlin":
		res = []*regexp.Regexp{reSpringRoute}
	case "C#":
		res = []*regexp.Regexp{reASPRoute}
	case "PHP":
		res = []*regexp.Regexp{rePHPRoute}
	case "Ruby":
		res = []*regexp.Regexp{reRubyRoute}
	default:
		return nil
	}
	var routes []Route
	for i, line := range strings.Split(text, "\n") {
		for _, re := range res {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				method, p := strings.ToUpper(m[1]), m[len(m)-1]
				if re == reGoRoute {
					method = ""
					if mp := reMethodPath.FindStringSubmatch(p); mp != nil {
						method, p = mp[1], mp[2]
					}
					if !strings.HasPrefix(p, "/") {
						continue
					}
				}
				if method == "REQUEST" || method == "ROUTE" || method == "ALL" || method == "ANY" || method == "API_ROUTE" {
					method = ""
				}
				routes = append(routes, Route{Method: method, Path: p, Line: i + 1})
			}
		}
	}
	return routes
}
