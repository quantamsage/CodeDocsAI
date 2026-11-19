package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/quantamsage/ai-code-documentation-tool/internal/ai"
	"github.com/quantamsage/ai-code-documentation-tool/internal/files"
	"github.com/quantamsage/ai-code-documentation-tool/internal/jobs"
	"github.com/quantamsage/ai-code-documentation-tool/internal/projects"
	"github.com/quantamsage/ai-code-documentation-tool/internal/structure"
)

// JobType is the job type that runs an analysis.
const JobType = "analyze_project"

// maxContentBytes caps how much of a file is read for analysis.
const maxContentBytes = 4 << 20

// ErrNoFiles fails an analysis of a project without relevant files.
var ErrNoFiles = errors.New("analysis: the project has no files to analyze")

// Analyzer runs the analysis pipeline as a background job.
type Analyzer struct {
	Files    *files.Store
	Store    *Store
	Provider ai.Provider // nil when no AI is configured
	// BatchTokens is the approximate input-token budget per request.
	BatchTokens int
	Log         *slog.Logger
}

// Definition returns the job definition for JobType.
func (a *Analyzer) Definition() jobs.Definition {
	return jobs.Definition{Run: a.run, OnFailed: a.onFailed}
}

func (a *Analyzer) budget() int {
	if a.BatchTokens < 2000 {
		return 40000
	}
	return a.BatchTokens
}

func (a *Analyzer) log() *slog.Logger {
	if a.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.Log
}

func (a *Analyzer) onFailed(ctx context.Context, job jobs.Job) {
	status := projects.StatusFailed
	if job.Status == jobs.StatusCanceled {
		status = projects.StatusUploaded
	}
	if err := a.Store.setProjectStatus(ctx, job.ProjectID, status); err != nil {
		a.log().Error("reset project status", slog.Int64("project_id", job.ProjectID), slog.String("error", err.Error()))
	}
}

func (a *Analyzer) run(ctx context.Context, job jobs.Job, progress jobs.Progress) error {
	if a.Provider == nil {
		return &ai.Error{Kind: ai.ErrNotConfigured, Message: "AI analysis is not available: no AI API key is configured."}
	}
	all, err := a.Files.List(ctx, job.UserID, job.ProjectID)
	if err != nil {
		return err
	}
	var selected []files.File
	for _, f := range all {
		if Relevant(f) {
			selected = append(selected, f)
		}
	}
	if len(selected) == 0 {
		return ErrNoFiles
	}
	sort.SliceStable(selected, func(i, j int) bool { return priority(selected[i]) < priority(selected[j]) })

	if err := a.Store.clearOtherRuns(ctx, job.ProjectID, job.ID); err != nil {
		return err
	}
	if err := a.Store.setProjectStatus(ctx, job.ProjectID, projects.StatusAnalyzing); err != nil {
		return err
	}

	// Steps 1-2: read files and extract structure (reusing this job's
	// earlier results when the job is being retried).
	progress.Update(0, 1, fmt.Sprintf("Extracting structure from %d files", len(selected))) //nolint:errcheck
	known := map[int64]structure.FileStructure{}
	if err := a.Store.jobResults(ctx, job.ProjectID, job.ID, "structure", func(id int64, raw []byte) error {
		var fs structure.FileStructure
		if json.Unmarshal(raw, &fs) == nil {
			known[id] = fs
		}
		return nil
	}); err != nil {
		return err
	}
	units := make([]unit, 0, len(selected))
	var modulePath string
	for _, f := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, _, err := a.Files.Content(f, maxContentBytes)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Path, err)
		}
		fs, ok := known[f.ID]
		if !ok {
			fs = structure.Extract(f.Path, f.Language, content)
			id := f.ID
			if err := a.Store.save(ctx, job.ProjectID, job.ID, &id, "structure", fs, "", 0, 0); err != nil {
				return err
			}
		}
		if strings.EqualFold(path.Base(f.Path), "go.mod") && modulePath == "" {
			modulePath = goModulePath(string(content))
		}
		units = append(units, unit{FileID: f.ID, Path: f.Path, Language: f.Language, Content: string(content), Structure: fs,
			Tokens: ai.EstimateTokens(string(content)) + ai.EstimateTokens(fs.Summary())})
	}

	// Steps 3-5: batch, analyze, validate and store each file.
	done, err := a.doneFiles(ctx, job)
	if err != nil {
		return err
	}
	var todo []unit
	for _, u := range units {
		if _, ok := done[u.FileID]; !ok {
			todo = append(todo, u)
		}
	}
	batches := planBatches(todo, a.budget())
	total := len(batches) + 1
	pending := map[string][]FileAnalysis{} // parts of split files
	for i, b := range batches {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.Update(i, total, fmt.Sprintf("Analyzing files (batch %d of %d: %s)", i+1, len(batches), describe(b))) //nolint:errcheck
		results, err := a.analyzeBatch(ctx, job, b, true)
		if err != nil {
			return err
		}
		for _, u := range b {
			fa := results[u.Path]
			if u.Parts > 1 {
				pending[u.Path] = append(pending[u.Path], fa)
				if len(pending[u.Path]) < u.Parts {
					continue
				}
				merged := pending[u.Path][0]
				for _, part := range pending[u.Path][1:] {
					merged = mergeParts(merged, part)
				}
				merged.Parts = u.Parts
				fa = merged
				delete(pending, u.Path)
			}
			id := u.FileID
			if err := a.Store.save(ctx, job.ProjectID, job.ID, &id, "file", fa, "", 0, 0); err != nil {
				return err
			}
			done[u.FileID] = fa
		}
	}

	// Steps 6-7: synthesize the project view and relationships.
	progress.Update(total-1, total, "Building the project overview") //nolint:errcheck
	structs := make([]structure.FileStructure, 0, len(units))
	for _, u := range units {
		structs = append(structs, u.Structure)
	}
	project, err := a.synthesize(ctx, job, all, units, done, structs, modulePath)
	if err != nil {
		return err
	}
	if err := a.Store.save(ctx, job.ProjectID, job.ID, nil, "project", project.analysis, project.model, project.in, project.out); err != nil {
		return err
	}
	return a.Store.setProjectStatus(ctx, job.ProjectID, projects.StatusAnalyzed)
}

func (a *Analyzer) doneFiles(ctx context.Context, job jobs.Job) (map[int64]FileAnalysis, error) {
	done := map[int64]FileAnalysis{}
	err := a.Store.jobResults(ctx, job.ProjectID, job.ID, "file", func(id int64, raw []byte) error {
		var fa FileAnalysis
		if err := json.Unmarshal(raw, &fa); err != nil {
			return err
		}
		done[id] = fa
		return nil
	})
	return done, err
}

func describe(b batch) string {
	if len(b) == 1 {
		if b[0].Parts > 1 {
			return fmt.Sprintf("%s, part %d of %d", b[0].Path, b[0].Part, b[0].Parts)
		}
		return b[0].Path
	}
	return fmt.Sprintf("%d files in %s", len(b), path.Dir(b[0].Path))
}

// analyzeBatch analyzes a batch. If the response is unusable (for example
// cut off by the output limit) a multi-file batch is split in two and
// retried; files the response omitted are retried on their own once.
func (a *Analyzer) analyzeBatch(ctx context.Context, job jobs.Job, b batch, retryMissing bool) (map[string]FileAnalysis, error) {
	resp, err := a.Provider.GenerateJSON(ctx, ai.Request{
		System: fileSystemPrompt,
		Prompt: filePrompt(b),
		Schema: fileSchema(),
		Label:  "analyze " + describe(b),
	})
	if err != nil {
		splittable := errors.Is(err, ai.ErrInvalidOutput) || errors.Is(err, ai.ErrRequestRejected)
		if splittable && len(b) > 1 {
			a.log().Info("splitting analysis batch", slog.Int64("job_id", job.ID), slog.Int("files", len(b)), slog.String("reason", err.Error()))
			left, right := halves(b)
			first, err := a.analyzeBatch(ctx, job, left, retryMissing)
			if err != nil {
				return nil, err
			}
			second, err := a.analyzeBatch(ctx, job, right, retryMissing)
			if err != nil {
				return nil, err
			}
			for k, v := range second {
				first[k] = v
			}
			return first, nil
		}
		return nil, err
	}
	results, err := validateFiles(resp.JSON, b)
	if err != nil {
		return nil, &ai.Error{Kind: ai.ErrInvalidOutput, Message: "The AI returned an analysis that could not be read.", Err: err}
	}
	meta := map[string]any{"paths": paths(b), "returned": len(results)}
	if err := a.Store.save(ctx, job.ProjectID, job.ID, nil, "batch", meta, resp.Model, resp.InputTokens, resp.OutputTokens); err != nil {
		return nil, err
	}
	for _, u := range b {
		if _, ok := results[u.Path]; ok {
			continue
		}
		if retryMissing && len(b) > 1 {
			single, err := a.analyzeBatch(ctx, job, batch{u}, false)
			if err != nil {
				return nil, err
			}
			if fa, ok := single[u.Path]; ok {
				results[u.Path] = fa
				continue
			}
		}
		results[u.Path] = FileAnalysis{Path: u.Path, Language: u.Language, Missing: true, Parts: max(u.Parts, 1)}
	}
	return results, nil
}

func paths(b batch) []string {
	out := make([]string, len(b))
	for i, u := range b {
		out[i] = u.Key()
	}
	return out
}

type synthesis struct {
	analysis ProjectAnalysis
	model    string
	in, out  int
}

type rawProject struct {
	Purpose            string         `json:"purpose"`
	MainFunctionality  []string       `json:"main_functionality"`
	Technologies       []string       `json:"technologies"`
	Architecture       string         `json:"architecture"`
	Components         []Component    `json:"components"`
	Workflows          []Workflow     `json:"workflows"`
	EntryPoints        []string       `json:"entry_points"`
	Interactions       []Relationship `json:"interactions"`
	ErrorHandling      string         `json:"error_handling"`
	Logging            string         `json:"logging"`
	DependencyPurposes []struct {
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
	} `json:"dependency_purposes"`
	Assumptions []string `json:"assumptions"`
}

// synthesize builds the project analysis: aggregates computed from files
// and structure, plus an AI synthesis from per-file summaries (or module
// summaries when the file summaries exceed the token budget).
func (a *Analyzer) synthesize(ctx context.Context, job jobs.Job, all []files.File, units []unit, done map[int64]FileAnalysis, structs []structure.FileStructure, modulePath string) (synthesis, error) {
	pa := aggregate(all, units, done)
	pa.ImportGraph = importGraph(structs, modulePath)
	var syn synthesis

	modules := moduleGroups(units, done)
	facts := projectFacts(pa, modules, nil)
	if ai.EstimateTokens(facts) > a.budget() {
		summaries, model, in, out, err := a.summarizeModules(ctx, modules)
		if err != nil {
			return syn, err
		}
		pa.Modules = summaries
		syn.model, syn.in, syn.out = model, in, out
		facts = projectFacts(pa, nil, summaries)
	}

	resp, err := a.Provider.GenerateJSON(ctx, ai.Request{System: projectSystemPrompt, Prompt: facts, Schema: projectSchema(), Label: "project synthesis"})
	if err != nil {
		return syn, err
	}
	var raw rawProject
	if err := json.Unmarshal(resp.JSON, &raw); err != nil {
		return syn, &ai.Error{Kind: ai.ErrInvalidOutput, Message: "The AI returned a project overview that could not be read.", Err: err}
	}
	knownPaths := map[string]bool{}
	for _, u := range units {
		knownPaths[u.Path] = true
		for d := path.Dir(u.Path); d != "."; d = path.Dir(d) {
			knownPaths[d] = true
		}
	}
	pa.Purpose = clean(raw.Purpose, maxSummary)
	pa.MainFunctionality = cleanList(raw.MainFunctionality, 30, maxText)
	pa.Technologies = cleanList(raw.Technologies, 40, maxShort)
	pa.Architecture = clean(raw.Architecture, 2000)
	for _, c := range raw.Components {
		comp := Component{Name: clean(c.Name, maxShort), Responsibility: clean(c.Responsibility, maxText)}
		for _, p := range c.Paths {
			if p = strings.Trim(clean(p, maxShort), "/"); knownPaths[p] {
				comp.Paths = append(comp.Paths, p)
			}
		}
		if comp.Name != "" && len(pa.Components) < 40 {
			pa.Components = append(pa.Components, comp)
		}
	}
	for _, w := range raw.Workflows {
		if name := clean(w.Name, maxShort); name != "" && len(pa.Workflows) < 20 {
			pa.Workflows = append(pa.Workflows, Workflow{Name: name, Steps: cleanList(w.Steps, 20, maxText)})
		}
	}
	pa.EntryPoints = cleanList(raw.EntryPoints, 20, maxShort)
	for _, r := range raw.Interactions {
		if t := clean(r.Target, maxShort); t != "" && len(pa.Interactions) < 60 {
			pa.Interactions = append(pa.Interactions, Relationship{From: clean(r.From, maxShort), Target: t, Kind: clean(r.Kind, 40), Description: clean(r.Description, maxText)})
		}
	}
	pa.ErrorHandling = clean(raw.ErrorHandling, 1500)
	pa.Logging = clean(raw.Logging, 1500)
	pa.Assumptions = cleanList(raw.Assumptions, maxAssumptions, maxText)
	purposes := map[string]string{}
	for _, d := range raw.DependencyPurposes {
		purposes[strings.ToLower(clean(d.Name, maxShort))] = clean(d.Purpose, maxText)
	}
	for i := range pa.Dependencies {
		pa.Dependencies[i].Purpose = purposes[strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(pa.Dependencies[i].Name, " (dev)"), " (indirect)"))]
	}

	syn.analysis = pa
	if syn.model == "" {
		syn.model = resp.Model
	}
	syn.in += resp.InputTokens
	syn.out += resp.OutputTokens
	return syn, nil
}

// aggregate collects facts that need no AI: languages, declared and used
// dependencies, endpoints, data models and configuration.
func aggregate(all []files.File, units []unit, done map[int64]FileAnalysis) ProjectAnalysis {
	pa := ProjectAnalysis{FileCount: len(all)}
	langs := map[string]int{}
	for _, f := range all {
		if f.Kind == files.KindSource && f.Language != "" {
			langs[f.Language]++
		}
	}
	for lang, n := range langs {
		pa.Languages = append(pa.Languages, LanguageShare{Language: lang, Files: n})
	}
	sort.Slice(pa.Languages, func(i, j int) bool {
		if pa.Languages[i].Files != pa.Languages[j].Files {
			return pa.Languages[i].Files > pa.Languages[j].Files
		}
		return pa.Languages[i].Language < pa.Languages[j].Language
	})

	deps := map[string]*Dependency{}
	var order []string
	addDep := func(name, usedIn string, declared bool) {
		key := strings.ToLower(name)
		d, ok := deps[key]
		if !ok {
			d = &Dependency{Name: name}
			deps[key] = d
			order = append(order, key)
		}
		d.Declared = d.Declared || declared
		if usedIn != "" && len(d.UsedIn) < 20 && !contains(d.UsedIn, usedIn) {
			d.UsedIn = append(d.UsedIn, usedIn)
		}
	}
	for _, u := range units {
		for _, dep := range u.Structure.Dependencies {
			addDep(dep, u.Path, true)
		}
	}
	endpointSeen := map[string]bool{}
	modelSeen := map[string]bool{}
	configSeen := map[string]bool{}
	for _, u := range units {
		fa, ok := done[u.FileID]
		if !ok {
			continue
		}
		if !fa.Missing {
			pa.AnalyzedFiles++
		}
		for _, dep := range fa.Dependencies {
			if len(order) < 200 {
				addDep(dep, u.Path, false)
			}
		}
		for _, e := range fa.Endpoints {
			key := e.Method + " " + e.Path
			if !endpointSeen[key] && len(pa.Endpoints) < 300 {
				endpointSeen[key] = true
				pa.Endpoints = append(pa.Endpoints, e)
			}
		}
		for _, m := range fa.DataModels {
			if key := strings.ToLower(m.Name); !modelSeen[key] && len(pa.DataModels) < 200 {
				modelSeen[key] = true
				pa.DataModels = append(pa.DataModels, m)
			}
		}
		for _, c := range fa.Configuration {
			if !configSeen[c.Name] && len(pa.Configuration) < 200 {
				configSeen[c.Name] = true
				pa.Configuration = append(pa.Configuration, c)
			}
		}
	}
	for _, key := range order {
		pa.Dependencies = append(pa.Dependencies, *deps[key])
	}
	return pa
}

func contains(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

// module is a directory and the summaries of its files.
type module struct {
	Path  string
	Lines []string
}

func moduleGroups(units []unit, done map[int64]FileAnalysis) []module {
	byDir := map[string]*module{}
	var order []string
	seen := map[int64]bool{}
	for _, u := range units {
		if seen[u.FileID] {
			continue
		}
		seen[u.FileID] = true
		dir := path.Dir(u.Path)
		m, ok := byDir[dir]
		if !ok {
			m = &module{Path: dir}
			byDir[dir] = m
			order = append(order, dir)
		}
		fa := done[u.FileID]
		line := u.Path + " (" + u.Language + "): "
		if fa.Missing || fa.Summary == "" {
			line += "(no analysis available)"
		} else {
			line += fa.Summary
			if len(fa.Responsibilities) > 0 {
				line += " Responsibilities: " + strings.Join(fa.Responsibilities, "; ")
			}
		}
		m.Lines = append(m.Lines, line)
	}
	sort.Strings(order)
	out := make([]module, 0, len(order))
	for _, dir := range order {
		out = append(out, *byDir[dir])
	}
	return out
}

func projectFacts(pa ProjectAnalysis, modules []module, summaries []ModuleSummary) string {
	var b strings.Builder
	b.WriteString("<project_facts>\nlanguages: ")
	for i, l := range pa.Languages {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%d files)", l.Language, l.Files)
	}
	fmt.Fprintf(&b, "\nfiles: %d total, %d analyzed\n", pa.FileCount, pa.AnalyzedFiles)
	if len(pa.Dependencies) > 0 {
		b.WriteString("dependencies: ")
		for i, d := range pa.Dependencies {
			if i == 120 {
				b.WriteString("…")
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(d.Name)
		}
		b.WriteString("\n")
	}
	if len(pa.Endpoints) > 0 {
		b.WriteString("api endpoints:\n")
		for i, e := range pa.Endpoints {
			if i == 80 {
				b.WriteString("- …\n")
				break
			}
			fmt.Fprintf(&b, "- %s %s (%s in %s)\n", e.Method, e.Path, e.Handler, e.File)
		}
	}
	if len(pa.DataModels) > 0 {
		b.WriteString("data models: ")
		for i, m := range pa.DataModels {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(m.Name + " (" + m.File + ")")
		}
		b.WriteString("\n")
	}
	if len(pa.Configuration) > 0 {
		b.WriteString("configuration: ")
		for i, c := range pa.Configuration {
			if i == 80 {
				b.WriteString("…")
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.Name)
		}
		b.WriteString("\n")
	}
	if len(pa.ImportGraph) > 0 {
		b.WriteString("internal imports (from -> to):\n")
		for i, r := range pa.ImportGraph {
			if i == 150 {
				b.WriteString("- …\n")
				break
			}
			fmt.Fprintf(&b, "- %s -> %s\n", r.From, r.Target)
		}
	}
	for _, m := range modules {
		fmt.Fprintf(&b, "\nmodule %s:\n", m.Path)
		for _, line := range m.Lines {
			b.WriteString("- " + line + "\n")
		}
	}
	if len(summaries) > 0 {
		b.WriteString("\nmodules:\n")
		for _, s := range summaries {
			fmt.Fprintf(&b, "- %s: %s\n", s.Path, s.Responsibility)
		}
	}
	b.WriteString("</project_facts>\n\nWrite the project-level analysis.")
	return b.String()
}

// summarizeModules asks for one-line module summaries, in groups that fit
// the token budget.
func (a *Analyzer) summarizeModules(ctx context.Context, modules []module) ([]ModuleSummary, string, int, int, error) {
	var out []ModuleSummary
	var model string
	var in, outTokens int
	var group []module
	groupTokens := 0
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		var b strings.Builder
		b.WriteString("<modules>\n")
		for _, m := range group {
			fmt.Fprintf(&b, "<module path=%q>\n", m.Path)
			for _, line := range m.Lines {
				b.WriteString("- " + line + "\n")
			}
			b.WriteString("</module>\n")
		}
		b.WriteString("</modules>\n\nSummarize each module.")
		resp, err := a.Provider.GenerateJSON(ctx, ai.Request{System: moduleSystemPrompt, Prompt: b.String(), Schema: moduleSchema(), Label: "module summaries"})
		if err != nil {
			return err
		}
		var raw struct {
			Modules []ModuleSummary `json:"modules"`
		}
		if err := json.Unmarshal(resp.JSON, &raw); err != nil {
			return &ai.Error{Kind: ai.ErrInvalidOutput, Message: "The AI returned module summaries that could not be read.", Err: err}
		}
		wanted := map[string]bool{}
		for _, m := range group {
			wanted[m.Path] = true
		}
		for _, s := range raw.Modules {
			if wanted[s.Path] {
				out = append(out, ModuleSummary{Path: s.Path, Responsibility: clean(s.Responsibility, maxText)})
				delete(wanted, s.Path)
			}
		}
		model = resp.Model
		in += resp.InputTokens
		outTokens += resp.OutputTokens
		group, groupTokens = nil, 0
		return nil
	}
	for _, m := range modules {
		// A huge module is trimmed to fit; its files were analyzed individually.
		lines := m.Lines
		for ai.EstimateTokens(strings.Join(lines, "\n")) > a.budget()*8/10 && len(lines) > 1 {
			lines = lines[:len(lines)*3/4]
		}
		m.Lines = lines
		t := ai.EstimateTokens(strings.Join(m.Lines, "\n"))
		if groupTokens+t > a.budget()*8/10 {
			if err := flush(); err != nil {
				return nil, "", 0, 0, err
			}
		}
		group = append(group, m)
		groupTokens += t
	}
	if err := flush(); err != nil {
		return nil, "", 0, 0, err
	}
	return out, model, in, outTokens, nil
}

// Classify maps errors from analysis (and documentation) jobs to retry
// decisions and user-facing messages.
func Classify(err error) jobs.Outcome {
	switch {
	case errors.Is(err, ErrNoFiles):
		return jobs.Outcome{Message: "The project has no source, configuration or documentation files to analyze. Upload code first."}
	case errors.Is(err, ai.ErrNotConfigured):
		return jobs.Outcome{Message: ai.UserMessage(err) + " Ask the administrator to set ANTHROPIC_API_KEY."}
	case ai.Retryable(err):
		return jobs.Outcome{Retry: true, RetryAfter: ai.RetryAfter(err), Message: ai.UserMessage(err)}
	case errors.Is(err, ai.ErrRequestRejected):
		return jobs.Outcome{Message: ai.UserMessage(err)}
	}
	return jobs.Outcome{Message: "The analysis failed because of an internal error."}
}
