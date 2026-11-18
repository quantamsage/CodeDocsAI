// Package files handles uploaded project files: path validation, language and
// kind detection, safe ZIP extraction, on-disk storage and the owner-scoped
// file store used by the project explorer. Uploaded code is only ever read as
// data; it is never executed.
package files

import (
	"bytes"
	"path"
	"strings"
	"unicode/utf8"
)

// File kinds (mirrors the CHECK constraint on project_files.kind).
const (
	KindSource        = "source"
	KindConfig        = "config"
	KindManifest      = "manifest"
	KindDocumentation = "documentation"
	KindOther         = "other"
)

// Classification describes what a file is.
type Classification struct {
	// Language is the programming, markup or data language ("Go", "YAML").
	Language string
	// Kind is one of the Kind constants.
	Kind string
}

// sourceExtensions maps extensions of programming-language source files.
var sourceExtensions = map[string]string{
	".go": "Go",
	".py": "Python", ".pyi": "Python", ".pyw": "Python",
	".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".jsx": "JavaScript",
	".ts": "TypeScript", ".tsx": "TypeScript", ".mts": "TypeScript", ".cts": "TypeScript",
	".java": "Java",
	".c":    "C", ".h": "C",
	".cpp": "C++", ".cc": "C++", ".cxx": "C++", ".c++": "C++", ".hpp": "C++", ".hh": "C++", ".hxx": "C++", ".h++": "C++", ".ipp": "C++", ".tpp": "C++",
	".rs":  "Rust",
	".php": "PHP", ".phtml": "PHP",
	".rb": "Ruby", ".rake": "Ruby", ".erb": "Ruby",
	".cs": "C#", ".csx": "C#",
	".kt": "Kotlin", ".kts": "Kotlin",
	".swift": "Swift",
	".scala": "Scala",
	".dart":  "Dart",
	".lua":   "Lua",
	".r":     "R",
	".pl":    "Perl", ".pm": "Perl",
	".sh": "Shell", ".bash": "Shell", ".zsh": "Shell",
	".ps1": "PowerShell", ".psm1": "PowerShell",
	".sql":  "SQL",
	".html": "HTML", ".htm": "HTML", ".tmpl": "Go template", ".gohtml": "Go template",
	".css": "CSS", ".scss": "SCSS", ".sass": "Sass", ".less": "Less",
	".vue": "Vue", ".svelte": "Svelte",
	".proto": "Protocol Buffers", ".graphql": "GraphQL", ".gql": "GraphQL",
}

// configExtensions maps extensions of configuration and data files.
var configExtensions = map[string]string{
	".json": "JSON", ".jsonc": "JSON",
	".yaml": "YAML", ".yml": "YAML",
	".toml": "TOML",
	".ini":  "INI", ".cfg": "INI", ".conf": "Config", ".properties": "Properties",
	".xml": "XML",
	".env": "dotenv",
}

// docExtensions maps documentation formats.
var docExtensions = map[string]string{
	".md": "Markdown", ".markdown": "Markdown",
	".rst": "reStructuredText", ".adoc": "AsciiDoc", ".txt": "Text",
}

// manifestNames are dependency manifests and lock files, by exact base name.
var manifestNames = map[string]string{
	"go.mod": "Go", "go.sum": "Go", "go.work": "Go",
	"package.json": "JavaScript", "package-lock.json": "JavaScript", "yarn.lock": "JavaScript",
	"pnpm-lock.yaml": "JavaScript", "npm-shrinkwrap.json": "JavaScript",
	"requirements.txt": "Python", "requirements-dev.txt": "Python", "pyproject.toml": "Python",
	"pipfile": "Python", "pipfile.lock": "Python", "poetry.lock": "Python", "setup.py": "Python",
	"setup.cfg": "Python", "environment.yml": "Python", "uv.lock": "Python",
	"pom.xml": "Java", "build.gradle": "Java", "build.gradle.kts": "Kotlin", "settings.gradle": "Java",
	"settings.gradle.kts": "Kotlin", "gradle.properties": "Java",
	"cargo.toml": "Rust", "cargo.lock": "Rust",
	"composer.json": "PHP", "composer.lock": "PHP",
	"gemfile": "Ruby", "gemfile.lock": "Ruby",
	"packages.config": "C#", "directory.packages.props": "C#", "directory.build.props": "C#",
	"cmakelists.txt": "CMake", "conanfile.txt": "C++", "conanfile.py": "C++", "vcpkg.json": "C++",
	"meson.build": "Meson",
}

// manifestExtensions are manifests identified by extension.
var manifestExtensions = map[string]string{
	".csproj": "C#", ".fsproj": "F#", ".vbproj": "Visual Basic", ".sln": "C#", ".gemspec": "Ruby", ".cabal": "Haskell",
}

// configNames are configuration files without a telling extension.
var configNames = map[string]string{
	"dockerfile": "Dockerfile", "containerfile": "Dockerfile", "makefile": "Makefile", "gnumakefile": "Makefile",
	"procfile": "Procfile", "vagrantfile": "Ruby", "rakefile": "Ruby", "justfile": "Just",
	".gitignore": "gitignore", ".gitattributes": "gitattributes", ".dockerignore": "gitignore",
	".editorconfig": "EditorConfig", ".npmrc": "INI", ".nvmrc": "Text", ".python-version": "Text",
	".ruby-version": "Text", ".tool-versions": "Text", ".prettierrc": "JSON", ".eslintrc": "JSON",
	".babelrc": "JSON", "codeowners": "Text",
}

// docNames are documentation files commonly written without an extension.
var docNames = map[string]bool{
	"readme": true, "license": true, "licence": true, "copying": true, "changelog": true,
	"authors": true, "contributors": true, "notice": true, "history": true,
}

// Classify identifies a file from its path. ok is false for file types the
// application does not accept (binaries, media, archives, unknown formats).
func Classify(p string) (c Classification, ok bool) {
	base := strings.ToLower(path.Base(p))
	ext := strings.ToLower(path.Ext(base))

	if lang, found := manifestNames[base]; found {
		return Classification{Language: lang, Kind: KindManifest}, true
	}
	if lang, found := manifestExtensions[ext]; found {
		return Classification{Language: lang, Kind: KindManifest}, true
	}
	if lang, found := configNames[base]; found {
		return Classification{Language: lang, Kind: KindConfig}, true
	}
	if strings.HasPrefix(base, ".env.") && isEnvTemplate(base) {
		return Classification{Language: "dotenv", Kind: KindConfig}, true
	}
	if strings.HasPrefix(base, "dockerfile.") || strings.HasSuffix(base, ".dockerfile") {
		return Classification{Language: "Dockerfile", Kind: KindConfig}, true
	}
	if lang, found := sourceExtensions[ext]; found {
		return Classification{Language: lang, Kind: KindSource}, true
	}
	if lang, found := configExtensions[ext]; found {
		return Classification{Language: lang, Kind: KindConfig}, true
	}
	if lang, found := docExtensions[ext]; found {
		return Classification{Language: lang, Kind: KindDocumentation}, true
	}
	if ext == "" && docNames[base] {
		return Classification{Language: "Text", Kind: KindDocumentation}, true
	}
	return Classification{}, false
}

// IsSecretFile reports whether a file conventionally holds credentials
// (.env, .env.production, private keys). Such files are never stored, so
// they cannot leak into analysis prompts or exports. Templates such as
// .env.example are allowed.
func IsSecretFile(p string) bool {
	base := strings.ToLower(path.Base(p))
	if base == ".env" || (strings.HasPrefix(base, ".env.") && !isEnvTemplate(base)) {
		return true
	}
	switch path.Ext(base) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore":
		return true
	}
	switch base {
	case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", ".netrc", ".pgpass", "credentials.json":
		return true
	}
	return false
}

func isEnvTemplate(base string) bool {
	for _, suffix := range []string{".example", ".sample", ".template", ".dist", ".defaults"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// ignoredDirs are directories that hold dependencies, build output, VCS data
// or tooling state rather than the project's own code.
var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "__macosx": true,
	"node_modules": true, "bower_components": true, ".venv": true, "venv": true, "__pycache__": true,
	".mypy_cache": true, ".pytest_cache": true, ".tox": true, ".gradle": true, ".idea": true, ".vscode": true,
	".vs": true, ".next": true, ".nuxt": true, ".terraform": true, ".cache": true,
}

// IgnoredDir returns the first directory in p that is ignored, or "".
func IgnoredDir(p string) string {
	segs := strings.Split(p, "/")
	for _, seg := range segs[:len(segs)-1] {
		if ignoredDirs[strings.ToLower(seg)] {
			return seg
		}
	}
	return ""
}

// IsIgnoredFile reports OS metadata files that are never project content.
func IsIgnoredFile(p string) bool {
	base := path.Base(p)
	return base == ".DS_Store" || base == "Thumbs.db" || base == "desktop.ini" || strings.HasPrefix(base, "._")
}

// executableMagic are signatures of native executables and libraries.
var executableMagic = [][]byte{
	{0x7f, 'E', 'L', 'F'},    // ELF
	{'M', 'Z'},               // PE / DOS
	{0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32 (LE)
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64 (LE)
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal / Java class
	{0x00, 'a', 's', 'm'},    // WebAssembly
}

// archiveExtensions are archive formats that are never extracted when nested.
var archiveExtensions = map[string]bool{
	".zip": true, ".jar": true, ".war": true, ".ear": true, ".apk": true, ".tar": true, ".gz": true, ".tgz": true,
	".bz2": true, ".xz": true, ".7z": true, ".rar": true, ".zst": true, ".whl": true, ".nupkg": true,
}

// executableExtensions are native binaries and installers.
var executableExtensions = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".com": true, ".msi": true,
	".scr": true, ".sys": true, ".o": true, ".obj": true, ".a": true, ".lib": true, ".class": true,
	".pyc": true, ".pyo": true, ".wasm": true, ".app": true, ".deb": true, ".rpm": true, ".dmg": true,
}

// IsArchive reports whether p names an archive by extension.
func IsArchive(p string) bool { return archiveExtensions[strings.ToLower(path.Ext(p))] }

// IsExecutableName reports whether p names a native binary by extension.
func IsExecutableName(p string) bool { return executableExtensions[strings.ToLower(path.Ext(p))] }

// IsExecutableContent reports whether data starts with a native executable
// or bytecode signature.
func IsExecutableContent(data []byte) bool {
	for _, m := range executableMagic {
		if bytes.HasPrefix(data, m) {
			// "MZ" is also a plausible start of a text file; require the PE
			// header offset to be present before treating it as a binary.
			if m[0] == 'M' && (len(data) < 64 || !looksBinary(data[:64])) {
				continue
			}
			return true
		}
	}
	return false
}

// IsText reports whether data is UTF-8 text without NUL bytes. A UTF-8 BOM
// is allowed.
func IsText(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	return !looksBinary(data) && utf8.Valid(data)
}

func looksBinary(data []byte) bool { return bytes.IndexByte(data, 0) >= 0 }
