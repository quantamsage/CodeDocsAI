package analysis

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Limits on what is stored from model output.
const (
	maxSummary     = 800
	maxText        = 500
	maxShort       = 200
	maxListItems   = 60
	maxFunctions   = 120
	maxEndpoints   = 80
	maxAssumptions = 20
)

type rawFileResponse struct {
	Files []rawFile `json:"files"`
}

type rawFile struct {
	Path             string         `json:"path"`
	Summary          string         `json:"summary"`
	Responsibilities []string       `json:"responsibilities"`
	Functions        []Function     `json:"functions"`
	Types            []TypeInfo     `json:"types"`
	Dependencies     []string       `json:"dependencies"`
	Endpoints        []Endpoint     `json:"api_endpoints"`
	DataModels       []DataModel    `json:"data_models"`
	Configuration    []ConfigItem   `json:"configuration"`
	ErrorHandling    string         `json:"error_handling"`
	Logging          string         `json:"logging"`
	Relationships    []Relationship `json:"relationships"`
	Assumptions      []string       `json:"assumptions"`
}

// validateFiles parses a batch response and keeps only entries for files in
// the batch, and only claims that the code supports. Paths in the batch
// that the response does not cover are absent from the result.
func validateFiles(data []byte, b batch) (map[string]FileAnalysis, error) {
	var resp rawFileResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode analysis: %w", err)
	}
	byPath := map[string]unit{}
	for _, u := range b {
		byPath[u.Path] = u
	}
	out := map[string]FileAnalysis{}
	for _, raw := range resp.Files {
		u, ok := byPath[strings.TrimSpace(raw.Path)]
		if !ok {
			continue // not a file we sent
		}
		if _, dup := out[u.Path]; dup {
			continue
		}
		out[u.Path] = sanitizeFile(raw, u)
	}
	return out, nil
}

func sanitizeFile(raw rawFile, u unit) FileAnalysis {
	content := u.Content
	known := u.Structure.Names()
	fa := FileAnalysis{
		Path:             u.Path,
		Language:         u.Language,
		Summary:          clean(raw.Summary, maxSummary),
		Responsibilities: cleanList(raw.Responsibilities, maxListItems, maxShort),
		Dependencies:     cleanList(raw.Dependencies, maxListItems, maxShort),
		ErrorHandling:    clean(raw.ErrorHandling, maxText),
		Logging:          clean(raw.Logging, maxText),
		Assumptions:      cleanList(raw.Assumptions, maxAssumptions, maxText),
		Parts:            max(u.Parts, 1),
	}

	seen := map[string]bool{}
	for _, f := range raw.Functions {
		name := clean(f.Name, maxShort)
		short := name
		if i := strings.LastIndexAny(name, ".:"); i >= 0 {
			short = name[i+1:]
		}
		if short == "" || seen[name] || !appears(short, content) {
			continue // not defined in the code we sent: discard
		}
		seen[name] = true
		fa.Functions = append(fa.Functions, Function{
			Name:        name,
			Signature:   clean(f.Signature, maxText),
			Description: clean(f.Description, maxText),
			Parameters:  cleanList(f.Parameters, 30, maxShort),
			Returns:     clean(f.Returns, maxText),
			SideEffects: clean(f.SideEffects, maxText),
			Errors:      clean(f.Errors, maxText),
			Verified:    known[name] || known[short],
		})
		if len(fa.Functions) >= maxFunctions {
			break
		}
	}
	for _, t := range raw.Types {
		name := clean(t.Name, maxShort)
		if name == "" || !appears(name, content) || len(fa.Types) >= maxListItems {
			continue
		}
		fa.Types = append(fa.Types, TypeInfo{Name: name, Kind: clean(t.Kind, 40), Description: clean(t.Description, maxText), Verified: known[name]})
	}
	routePaths := map[string]bool{}
	for _, r := range u.Structure.Routes {
		routePaths[r.Path] = true
	}
	for _, e := range raw.Endpoints {
		p := clean(e.Path, maxShort)
		if p == "" || (!routePaths[p] && !strings.Contains(content, p)) || len(fa.Endpoints) >= maxEndpoints {
			continue
		}
		fa.Endpoints = append(fa.Endpoints, Endpoint{
			Method: strings.ToUpper(clean(e.Method, 10)), Path: p, Handler: clean(e.Handler, maxShort),
			Description: clean(e.Description, maxText), Auth: clean(e.Auth, maxShort), File: u.Path,
		})
	}
	for _, m := range raw.DataModels {
		name := clean(m.Name, maxShort)
		if name == "" || !strings.Contains(strings.ToLower(content), strings.ToLower(name)) || len(fa.DataModels) >= maxListItems {
			continue
		}
		fa.DataModels = append(fa.DataModels, DataModel{Name: name, Description: clean(m.Description, maxText), Fields: cleanList(m.Fields, 80, maxShort), File: u.Path})
	}
	for _, c := range raw.Configuration {
		name := clean(c.Name, maxShort)
		if name == "" || !strings.Contains(content, name) || len(fa.Configuration) >= maxListItems {
			continue
		}
		fa.Configuration = append(fa.Configuration, ConfigItem{Name: name, Description: clean(c.Description, maxText), File: u.Path})
	}
	for _, r := range raw.Relationships {
		target := clean(r.Target, maxShort)
		if target == "" || len(fa.Relationships) >= maxListItems {
			continue
		}
		fa.Relationships = append(fa.Relationships, Relationship{From: u.Path, Target: target, Kind: clean(r.Kind, 40), Description: clean(r.Description, maxText)})
	}
	return fa
}

// appears reports whether name occurs in content as a whole identifier.
func appears(name, content string) bool {
	re, err := regexp.Compile(`(^|[^\w$])` + regexp.QuoteMeta(name) + `($|[^\w$])`)
	if err != nil {
		return false
	}
	return re.MatchString(content)
}

// mergeParts combines analyses of a file's line ranges.
func mergeParts(a, b FileAnalysis) FileAnalysis {
	if a.Summary == "" {
		a.Summary = b.Summary
	}
	a.Responsibilities = dedupe(append(a.Responsibilities, b.Responsibilities...))
	have := map[string]bool{}
	for _, f := range a.Functions {
		have[f.Name] = true
	}
	for _, f := range b.Functions {
		if !have[f.Name] {
			a.Functions = append(a.Functions, f)
		}
	}
	a.Types = append(a.Types, b.Types...)
	a.Dependencies = dedupe(append(a.Dependencies, b.Dependencies...))
	a.Endpoints = append(a.Endpoints, b.Endpoints...)
	a.DataModels = append(a.DataModels, b.DataModels...)
	a.Configuration = append(a.Configuration, b.Configuration...)
	a.Relationships = append(a.Relationships, b.Relationships...)
	a.Assumptions = dedupe(append(a.Assumptions, b.Assumptions...))
	if a.ErrorHandling == "" {
		a.ErrorHandling = b.ErrorHandling
	}
	if a.Logging == "" {
		a.Logging = b.Logging
	}
	a.Missing = a.Missing && b.Missing
	return a
}

// clean trims, collapses whitespace, removes control characters and caps length.
func clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > limit {
		s = string(runes[:limit-1]) + "…"
	}
	return s
}

func cleanList(items []string, maxItems, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range items {
		item = clean(item, limit)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

func dedupe(items []string) []string {
	return cleanList(items, len(items)+1, 1<<20)
}
