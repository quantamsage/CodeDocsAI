// Package analysis runs the AI code-analysis pipeline for a project and
// stores its results.
//
// Pipeline (one background job per run):
//
//  1. Select relevant files (source, configuration, manifests, docs; no lock files).
//  2. Extract structure with the structure package (go/ast for Go).
//  3. Divide the files into batches that fit the token budget, splitting
//     large files into line ranges.
//  4. Analyze each batch with the AI, giving it the code and its structure.
//  5. Validate and store each file's analysis (resumable on retry).
//  6. Synthesize a project-level analysis from the per-file results (via
//     module summaries when the project is too large for one request).
//  7. Add relationships: a deterministic import graph plus the interactions
//     the AI describes.
//
// Uploaded code is untrusted: it is only ever placed in delimited data
// sections of prompts, never executed, and model output is validated against
// the extracted structure before it is stored.
package analysis

import "github.com/quantamsage/ai-code-documentation-tool/internal/structure"

// Function is one documented function or method.
type Function struct {
	Name        string   `json:"name"`
	Signature   string   `json:"signature"`
	Description string   `json:"description"`
	Parameters  []string `json:"parameters"`
	Returns     string   `json:"returns"`
	SideEffects string   `json:"side_effects"`
	Errors      string   `json:"errors"`
	// Verified is set when the name was found in the extracted structure.
	Verified bool `json:"verified"`
}

// TypeInfo is a class, struct, interface or other type.
type TypeInfo struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Verified    bool   `json:"verified"`
}

// Endpoint is an HTTP (or RPC) endpoint the code exposes.
type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Handler     string `json:"handler"`
	Description string `json:"description"`
	Auth        string `json:"auth"`
	File        string `json:"file,omitempty"`
}

// DataModel is a database table or persisted model.
type DataModel struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Fields      []string `json:"fields"`
	File        string   `json:"file,omitempty"`
}

// ConfigItem is an environment variable, flag or setting.
type ConfigItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
}

// Relationship links a file or component to another.
type Relationship struct {
	From        string `json:"from,omitempty"`
	Target      string `json:"target"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// FileAnalysis is the AI's analysis of one file, after validation.
type FileAnalysis struct {
	Path             string         `json:"path"`
	Language         string         `json:"language"`
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
	// Assumptions lists conclusions the code does not confirm directly.
	Assumptions []string `json:"assumptions"`
	// Parts is the number of line ranges the file was analyzed in (1 = whole file).
	Parts int `json:"parts"`
	// Missing is set when the AI returned no analysis for the file.
	Missing bool `json:"missing,omitempty"`
}

// Component is a major part of the system.
type Component struct {
	Name           string   `json:"name"`
	Paths          []string `json:"paths"`
	Responsibility string   `json:"responsibility"`
}

// Workflow is an important flow through the system.
type Workflow struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
}

// Dependency is an external library.
type Dependency struct {
	Name    string   `json:"name"`
	Purpose string   `json:"purpose"`
	UsedIn  []string `json:"used_in"`
	// Declared is set when the dependency appears in a manifest.
	Declared bool `json:"declared"`
}

// LanguageShare is a language and its number of source files.
type LanguageShare struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
}

// ModuleSummary describes one directory (module or package).
type ModuleSummary struct {
	Path           string `json:"path"`
	Responsibility string `json:"responsibility"`
}

// ProjectAnalysis is the synthesized, project-level understanding.
type ProjectAnalysis struct {
	Purpose           string          `json:"purpose"`
	MainFunctionality []string        `json:"main_functionality"`
	Technologies      []string        `json:"technologies"`
	Architecture      string          `json:"architecture"`
	Components        []Component     `json:"components"`
	Modules           []ModuleSummary `json:"modules"`
	Workflows         []Workflow      `json:"workflows"`
	EntryPoints       []string        `json:"entry_points"`
	ErrorHandling     string          `json:"error_handling"`
	Logging           string          `json:"logging"`
	Interactions      []Relationship  `json:"interactions"`
	Assumptions       []string        `json:"assumptions"`

	// Aggregated from files and structure (not written by the AI).
	Languages     []LanguageShare `json:"languages"`
	Dependencies  []Dependency    `json:"dependencies"`
	Endpoints     []Endpoint      `json:"api_endpoints"`
	DataModels    []DataModel     `json:"data_models"`
	Configuration []ConfigItem    `json:"configuration"`
	ImportGraph   []Relationship  `json:"import_graph"`
	FileCount     int             `json:"file_count"`
	AnalyzedFiles int             `json:"analyzed_files"`
}

// HasAPI reports whether the project exposes endpoints.
func (p ProjectAnalysis) HasAPI() bool { return len(p.Endpoints) > 0 }

// HasDatabase reports whether the project has data models.
func (p ProjectAnalysis) HasDatabase() bool { return len(p.DataModels) > 0 }

// StoredFile is a file's stored structure and analysis.
type StoredFile struct {
	FileID    int64
	Path      string
	Language  string
	Kind      string
	Structure structure.FileStructure
	Analysis  *FileAnalysis
}
