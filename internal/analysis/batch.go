package analysis

import (
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/quantamsage/ai-code-documentation-tool/internal/ai"
	"github.com/quantamsage/ai-code-documentation-tool/internal/files"
	"github.com/quantamsage/ai-code-documentation-tool/internal/structure"
)

// lockFiles are generated dependency locks: large and uninformative.
var lockFiles = map[string]bool{
	"go.sum": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"poetry.lock": true, "pipfile.lock": true, "cargo.lock": true, "composer.lock": true, "gemfile.lock": true, "uv.lock": true,
}

// Relevant reports whether a file is worth analyzing.
func Relevant(f files.File) bool {
	if lockFiles[strings.ToLower(path.Base(f.Path))] {
		return false
	}
	switch f.Kind {
	case files.KindSource, files.KindConfig, files.KindManifest:
		return true
	case files.KindDocumentation:
		// Project docs help explain purpose; licences and changelogs don't.
		name := strings.ToLower(path.Base(f.Path))
		return strings.HasPrefix(name, "readme") || strings.HasPrefix(name, "architecture") ||
			strings.HasPrefix(name, "contributing") || strings.Contains(path.Dir(f.Path), "doc")
	}
	return false
}

// priority orders files so manifests and READMEs come first in their directory.
func priority(f files.File) int {
	switch f.Kind {
	case files.KindManifest:
		return 0
	case files.KindDocumentation:
		return 1
	case files.KindConfig:
		return 2
	}
	return 3
}

// unit is one file, or one line range of a large file, to analyze.
type unit struct {
	FileID    int64
	Path      string
	Language  string
	Content   string
	Structure structure.FileStructure
	Part      int // 1-based; 0 when the whole file
	Parts     int
	FirstLine int
	Tokens    int
}

// Key identifies the unit across retries.
func (u unit) Key() string {
	if u.Parts <= 1 {
		return u.Path
	}
	return u.Path + "#" + strconv.Itoa(u.Part)
}

// batch is a group of units analyzed in one AI request.
type batch []unit

func (b batch) tokens() int {
	n := 0
	for _, u := range b {
		n += u.Tokens
	}
	return n
}

// maxFilesPerBatch keeps each response within the output-token limit.
const maxFilesPerBatch = 8

// planBatches turns files into batches within the token budget. Files
// larger than the budget are split into line ranges, each its own batch;
// smaller files are grouped by directory.
func planBatches(units []unit, budget int) []batch {
	sort.SliceStable(units, func(i, j int) bool {
		di, dj := path.Dir(units[i].Path), path.Dir(units[j].Path)
		if di != dj {
			return di < dj
		}
		return units[i].Path < units[j].Path
	})

	var batches []batch
	var current batch
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
		}
	}
	for _, u := range units {
		if u.Tokens > budget*7/10 {
			flush()
			for _, part := range split(u, budget*6/10) {
				batches = append(batches, batch{part})
			}
			continue
		}
		sameDir := len(current) == 0 || path.Dir(current[0].Path) == path.Dir(u.Path)
		if len(current) >= maxFilesPerBatch || current.tokens()+u.Tokens > budget || (!sameDir && current.tokens() > budget/2) {
			flush()
		}
		current = append(current, u)
	}
	flush()
	return batches
}

// split divides a unit into line ranges of at most maxTokens each.
func split(u unit, maxTokens int) []unit {
	lines := strings.SplitAfter(u.Content, "\n")
	var parts []unit
	var b strings.Builder
	first := 1
	for i, line := range lines {
		if b.Len() > 0 && ai.EstimateTokens(b.String()+line) > maxTokens {
			parts = append(parts, unit{FileID: u.FileID, Path: u.Path, Language: u.Language, Content: b.String(), Structure: u.Structure, FirstLine: first})
			b.Reset()
			first = i + 1
		}
		b.WriteString(line)
	}
	if b.Len() > 0 {
		parts = append(parts, unit{FileID: u.FileID, Path: u.Path, Language: u.Language, Content: b.String(), Structure: u.Structure, FirstLine: first})
	}
	for i := range parts {
		parts[i].Part, parts[i].Parts = i+1, len(parts)
		parts[i].Tokens = ai.EstimateTokens(parts[i].Content)
	}
	return parts
}

// halves splits a batch for a retry with smaller requests.
func halves(b batch) (batch, batch) {
	mid := len(b) / 2
	return append(batch(nil), b[:mid]...), append(batch(nil), b[mid:]...)
}
