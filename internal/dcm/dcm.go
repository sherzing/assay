// Package dcm ingests DCM (dcm.dev) JSON reports, so lens and strata read Dart and Flutter metrics.
// The decoder is partial on purpose, like the SARIF one: an unknown field cannot break the parse.
package dcm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// Version is the DCM formatVersion this importer was verified against (DCM 1.39).
const Version = 13

// Options controls an import.
type Options struct {
	// Root is the repository root; a generated file is recognised by its header when it is readable under Root.
	Root string
	// Prefix is the directory DCM ran in, relative to Root. DCM writes paths relative to its working
	// directory, so a report from one package of a monorepo needs it to key like the rest of the repository.
	Prefix string
}

// Result is what one report yields: per-declaration measures, never roll-ups.
type Result struct {
	FormatVersion int
	// Measures at function, class and file scope, keyed by the declaration's identity.
	Measures []schema.Measure
	// Functions are the declarations DCM computed cyclomatic complexity for, in the Go scan's shape.
	Functions []model.FuncMetrics
	Files     int // files measured
	Funcs     int // declarations with a per-function record
	Generated int // files skipped as generated code
	Located   int // report files found under Root; 0 means no generated-file header could be checked
	Issues    int // lint, unused-code and duplication issues in the report, which this importer does not read
}

type importer struct {
	opt     Options
	files   map[string]bool
	seen    map[string]bool
	fm      map[string]*entry
	nesting bool // the report measures nesting at all
	res     Result
}

// entry accumulates one declaration's per-function record.
type entry struct {
	f                   model.FuncMetrics
	cyclomatic, nesting bool
}

// Import reads one DCM JSON report.
func Import(r io.Reader, opt Options) (*Result, error) {
	root, err := readRoot(r)
	if err != nil {
		return nil, err
	}
	im := &importer{opt: opt, files: map[string]bool{}, seen: map[string]bool{}, fm: map[string]*entry{}}
	if err := im.header(root); err != nil {
		return nil, err
	}
	if err := im.sections(root); err != nil {
		return nil, err
	}
	im.functions()
	return &im.res, nil
}

// ImportFile reads a report from disk.
func ImportFile(path string, opt Options) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Import(f, opt)
}

// Merge unions several reports into one. Two reports disagreeing about one identity is an error,
// because it means they were run from different directories, not that the code has two values.
func Merge(rs ...*Result) (*Result, error) {
	if len(rs) == 1 {
		return rs[0], nil
	}
	u := &union{out: &Result{}, values: map[string]float64{}, funcs: map[string]bool{}, files: map[string]bool{}}
	for _, r := range rs {
		u.out.FormatVersion = max(u.out.FormatVersion, r.FormatVersion)
		u.out.Generated += r.Generated
		u.out.Located += r.Located
		u.out.Issues += r.Issues
		if err := u.measures(r.Measures); err != nil {
			return nil, err
		}
		u.functions(r.Functions)
	}
	sortFunctions(u.out.Functions)
	u.out.Files, u.out.Funcs = len(u.files), len(u.out.Functions)
	return u.out, nil
}

// union accumulates a Merge, keyed by identity.
type union struct {
	out    *Result
	values map[string]float64
	funcs  map[string]bool
	files  map[string]bool
}

func (u *union) measures(ms []schema.Measure) error {
	for _, m := range ms {
		k := string(m.Scope) + "\x00" + m.Path + "\x00" + m.Metric
		if v, ok := u.values[k]; ok {
			if v != m.Value {
				return fmt.Errorf("dcm: %s %s is %v in one report and %v in another; "+
					"were they run from different directories? see --prefix", m.Path, m.Metric, v, m.Value)
			}
			continue
		}
		u.values[k] = m.Value
		u.files[fileOf(m.Path)] = true
		u.out.Measures = append(u.out.Measures, m)
	}
	return nil
}

func (u *union) functions(fs []model.FuncMetrics) {
	for _, f := range fs {
		if k := f.Key(); !u.funcs[k] {
			u.funcs[k] = true
			u.out.Functions = append(u.out.Functions, f)
		}
	}
}

func readRoot(r io.Reader) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("dcm: not a JSON object: %w", err)
	}
	return root, nil
}

// header reads the version; no version means this is not a DCM report.
func (im *importer) header(root map[string]json.RawMessage) error {
	if raw, ok := root["formatVersion"]; ok {
		if err := json.Unmarshal(raw, &im.res.FormatVersion); err != nil {
			return fmt.Errorf("dcm: formatVersion: %w", err)
		}
	}
	if im.res.FormatVersion == 0 {
		return fmt.Errorf("dcm: no formatVersion in the root object; is this a DCM JSON report?")
	}
	return nil
}

// sections reads metricResults and counts the issues in the sections this importer does not read.
// A report with no metric values is an error: DCM measures only what analysis_options.yaml configures,
// and storing zeros for a repository would be a lie.
func (im *importer) sections(root map[string]json.RawMessage) error {
	files, err := readFiles(root["metricResults"])
	if err != nil {
		return fmt.Errorf("dcm: metricResults: %w", err)
	}
	im.values(files)
	switch {
	case len(im.res.Measures) > 0:
	case im.res.Generated > 0:
		return fmt.Errorf("dcm: every measured file is generated code (%d files); nothing to import", im.res.Generated)
	default:
		return fmt.Errorf("dcm: the report has no metric values: DCM measures only what analysis_options.yaml configures.\n" +
			"  Add a `dcm: metrics:` block (`dcm init metrics-preview --format=analysis_options lib` prints one) and run dcm with --metrics")
	}
	for _, sec := range []string{"analyzeResults", "unusedCodeResults", "unusedFilesResults", "duplicationResults"} {
		n, err := countIssues(root[sec])
		if err != nil {
			return fmt.Errorf("dcm: %s: %w", sec, err)
		}
		im.res.Issues += n
	}
	return nil
}

// functions lists the declarations DCM computed cyclomatic complexity for, in file order.
// A bodiless declaration has no complexity, so it gets no record and cannot fabricate a zero.
// DCM reports no nesting for a declaration whose only logic is an initialiser list; the Go scan
// says 0 for a flat function, so when the report measures nesting at all the measure says 0 too.
func (im *importer) functions() {
	var es []*entry
	for _, e := range im.fm {
		if e.cyclomatic {
			es = append(es, e)
		}
	}
	sort.Slice(es, func(i, j int) bool { return before(es[i].f, es[j].f) })
	for _, e := range es {
		im.res.Functions = append(im.res.Functions, e.f)
		if im.nesting && !e.nesting {
			im.res.Measures = append(im.res.Measures,
				schema.Measure{Scope: schema.ScopeFunction, Path: e.f.File + ":" + e.f.Name, Metric: "nesting"})
		}
	}
	im.res.Files, im.res.Funcs = len(im.files), len(im.res.Functions)
}

func sortFunctions(fs []model.FuncMetrics) {
	sort.Slice(fs, func(i, j int) bool { return before(fs[i], fs[j]) })
}

func before(a, b model.FuncMetrics) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Name < b.Name
}
