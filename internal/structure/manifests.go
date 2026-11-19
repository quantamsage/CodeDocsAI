package structure

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

var (
	reGoRequire    = regexp.MustCompile(`^\s*(?:require\s+)?([\w.\-]+\.[\w.\-/]+)\s+v[\w.\-+]+`)
	reRequirement  = regexp.MustCompile(`^\s*([A-Za-z0-9][\w.\-]*)(?:\[[^\]]*\])?\s*(?:[=<>!~;]|$)`)
	reGem          = regexp.MustCompile(`(?m)^\s*gem\s+['"]([^'"]+)['"]`)
	reTOMLSection  = regexp.MustCompile(`^\s*\[([^\]]+)\]`)
	reTOMLKey      = regexp.MustCompile(`^\s*([A-Za-z0-9_\-]+)\s*=`)
	reArtifactID   = regexp.MustCompile(`<artifactId>([^<]+)</artifactId>`)
	rePackageRef   = regexp.MustCompile(`<PackageReference\s+Include="([^"]+)"`)
	reGradleDep    = regexp.MustCompile(`(?m)^\s*(?:implementation|api|compileOnly|runtimeOnly|testImplementation)\s*\(?\s*['"]([^'"]+)['"]`)
	reCMakePackage = regexp.MustCompile(`(?i)find_package\(\s*(\w+)`)
)

// manifestDependencies extracts declared dependency names from known
// manifest files. ok is false for files that are not manifests.
func manifestDependencies(filePath, text string) (deps []string, ok bool) {
	switch base(filePath) {
	case "go.mod":
		inRequire := false
		for _, line := range lines(text) {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "require ("):
				inRequire = true
				continue
			case inRequire && trimmed == ")":
				inRequire = false
				continue
			}
			if (inRequire || strings.HasPrefix(trimmed, "require ")) && !strings.HasPrefix(trimmed, "//") {
				if m := reGoRequire.FindStringSubmatch(trimmed); m != nil {
					name := m[1]
					if strings.Contains(trimmed, "// indirect") {
						name += " (indirect)"
					}
					deps = append(deps, name)
				}
			}
		}
		return deps, true
	case "package.json":
		var pkg map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &pkg) != nil {
			return nil, true
		}
		for _, section := range []string{"dependencies", "devDependencies", "peerDependencies"} {
			var m map[string]string
			if json.Unmarshal(pkg[section], &m) == nil {
				names := make([]string, 0, len(m))
				for name := range m {
					if section == "devDependencies" {
						names = append(names, name+" (dev)")
					} else {
						names = append(names, name)
					}
				}
				sort.Strings(names)
				deps = append(deps, names...)
			}
		}
		return deps, true
	case "requirements.txt", "requirements-dev.txt":
		for _, line := range lines(text) {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
				continue
			}
			if m := reRequirement.FindStringSubmatch(trimmed); m != nil {
				deps = append(deps, m[1])
			}
		}
		return deps, true
	case "cargo.toml", "pyproject.toml":
		section := ""
		for _, line := range lines(text) {
			if m := reTOMLSection.FindStringSubmatch(line); m != nil {
				section = strings.ToLower(strings.TrimSpace(m[1]))
				continue
			}
			isDeps := strings.HasSuffix(section, "dependencies") || section == "tool.poetry.dependencies"
			if m := reTOMLKey.FindStringSubmatch(line); m != nil && isDeps && m[1] != "python" {
				deps = append(deps, m[1])
			}
		}
		return deps, true
	case "composer.json":
		var pkg map[string]map[string]string
		if json.Unmarshal([]byte(text), &pkg) != nil {
			return nil, true
		}
		for _, section := range []string{"require", "require-dev"} {
			names := make([]string, 0, len(pkg[section]))
			for name := range pkg[section] {
				if name != "php" && !strings.HasPrefix(name, "ext-") {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			deps = append(deps, names...)
		}
		return deps, true
	case "gemfile":
		return matchAll(reGem, text), true
	case "pom.xml":
		return matchAll(reArtifactID, text), true
	case "build.gradle", "build.gradle.kts":
		return matchAll(reGradleDep, text), true
	case "cmakelists.txt":
		return matchAll(reCMakePackage, text), true
	}
	if strings.HasSuffix(base(filePath), ".csproj") {
		return matchAll(rePackageRef, text), true
	}
	return nil, false
}

func matchAll(re *regexp.Regexp, text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func lines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}
