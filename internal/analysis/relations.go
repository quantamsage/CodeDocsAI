package analysis

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/quantamsage/ai-code-documentation-tool/internal/structure"
)

var reGoModule = regexp.MustCompile(`(?m)^\s*module\s+(\S+)`)

// goModulePath returns the module path declared in go.mod content.
func goModulePath(gomod string) string {
	if m := reGoModule.FindStringSubmatch(gomod); m != nil {
		return strings.Trim(m[1], `"`)
	}
	return ""
}

// importGraph resolves imports to files or packages inside the project. It
// is derived from the parsed structure only, so it contains no AI output.
func importGraph(structs []structure.FileStructure, modulePath string) []Relationship {
	paths := map[string]bool{}
	dirs := map[string]bool{}
	for _, fs := range structs {
		paths[fs.Path] = true
		for d := path.Dir(fs.Path); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	seen := map[string]bool{}
	var edges []Relationship
	add := func(from, target, kind string) {
		key := from + "->" + target
		if target == "" || target == from || target == path.Dir(from) || seen[key] {
			return
		}
		seen[key] = true
		edges = append(edges, Relationship{From: from, Target: target, Kind: kind})
	}
	for _, fs := range structs {
		for _, imp := range fs.Imports {
			switch fs.Language {
			case "Go":
				if modulePath != "" && strings.HasPrefix(imp, modulePath+"/") {
					if dir := strings.TrimPrefix(imp, modulePath+"/"); dirs[dir] {
						add(fs.Path, dir, "imports package")
					}
				}
			case "JavaScript", "TypeScript":
				if strings.HasPrefix(imp, ".") {
					add(fs.Path, resolveJS(path.Join(path.Dir(fs.Path), imp), paths, dirs), "imports")
				}
			case "Python":
				add(fs.Path, resolvePython(fs.Path, imp, paths), "imports")
			case "C", "C++":
				add(fs.Path, resolveInclude(fs.Path, imp, paths), "includes")
			case "Ruby":
				candidate := path.Join(path.Dir(fs.Path), imp)
				if !strings.HasSuffix(candidate, ".rb") {
					candidate += ".rb"
				}
				if paths[candidate] {
					add(fs.Path, candidate, "requires")
				}
			case "Java", "Kotlin":
				add(fs.Path, resolveBySuffix(strings.ReplaceAll(imp, ".", "/"), []string{".java", ".kt"}, paths), "imports")
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].Target < edges[j].Target
	})
	return edges
}

func resolveJS(p string, paths, dirs map[string]bool) string {
	exts := []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue", ".svelte"}
	bases := []string{p}
	if ext := path.Ext(p); ext == ".js" || ext == ".jsx" || ext == ".mjs" {
		bases = append(bases, strings.TrimSuffix(p, ext)) // TS sources imported as .js
	}
	for _, b := range bases {
		for _, ext := range exts {
			if paths[b+ext] {
				return b + ext
			}
		}
		for _, index := range []string{"/index.ts", "/index.tsx", "/index.js", "/index.jsx"} {
			if paths[b+index] {
				return b + index
			}
		}
	}
	if dirs[p] {
		return p
	}
	return ""
}

func resolvePython(from, imp string, paths map[string]bool) string {
	var base string
	if strings.HasPrefix(imp, ".") {
		dots := len(imp) - len(strings.TrimLeft(imp, "."))
		dir := path.Dir(from)
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		base = path.Join(dir, strings.ReplaceAll(strings.TrimLeft(imp, "."), ".", "/"))
	} else {
		base = strings.ReplaceAll(imp, ".", "/")
	}
	for _, prefix := range []string{"", "src/"} {
		for _, candidate := range []string{base + ".py", base + "/__init__.py"} {
			if paths[prefix+candidate] {
				return prefix + candidate
			}
		}
	}
	return ""
}

func resolveInclude(from, imp string, paths map[string]bool) string {
	if candidate := path.Join(path.Dir(from), imp); paths[candidate] {
		return candidate
	}
	return resolveBySuffix(imp, []string{""}, paths)
}

// resolveBySuffix finds the unique project file ending with name+ext.
func resolveBySuffix(name string, exts []string, paths map[string]bool) string {
	var match string
	for p := range paths {
		for _, ext := range exts {
			if p == name+ext || strings.HasSuffix(p, "/"+name+ext) {
				if match != "" && match != p {
					return "" // ambiguous
				}
				match = p
			}
		}
	}
	return match
}
