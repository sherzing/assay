package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/analyze"
	"github.com/sherzing/assay/internal/baseline"
	"github.com/sherzing/assay/internal/dcm"
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/report"
	"github.com/sherzing/assay/internal/sarif"
	"github.com/sherzing/assay/internal/verdict"
	"github.com/sherzing/assay/pkg/schema"
)

const importUsage = "usage: ratchet import [flags] <file.sarif | dcm.json>...\n" +
	"  several files are merged, which is what a multi-project build produces\n" +
	"  pipe with: golangci-lint run --out-format sarif | ratchet import -\n" +
	"  dart:      dcm run --metrics --report-all --no-fatal-found --reporter=json --output-to=dcm.json lib\n" +
	"             ratchet import dcm.json --root .\n" +
	"  monorepo:  ratchet import packages/app/dcm.json=packages/app packages/lib/dcm.json=packages/lib --root ."

// cmdImport ingests another tool's report: SARIF from any linter, or DCM's JSON for Dart metrics.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	var o importOpts
	fs.StringVar(&o.root, "root", ".", "repository root: report paths are relative to it")
	fs.StringVar(&o.format, "format", "auto", "input format: auto|sarif|dcm")
	fs.StringVar(&o.tool, "tool", "", "sarif: override the tool name used to namespace rule IDs")
	fs.StringVar(&o.prefix, "prefix", "", "dcm: the directory DCM ran in, relative to --root, prefixed to the report's paths")
	fs.StringVar(&o.file, "file", defaultBaselineFile, "baseline path")
	fs.StringVar(&o.mode, "mode", "report", "report | baseline | check")
	fs.BoolVar(&o.includeSuppressed, "include-suppressed", false, "sarif: import results the producer marked suppressed")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing baseline (mode=baseline)")
	fs.BoolVar(&o.asJSON, "json", false, "emit JSON")
	fs.StringVar(&o.emit, "emit", "", "emit assay JSONL instead of a report: measures|findings")
	fs.BoolVar(&o.detail, "detail", true, "emit=measures: include per-declaration records, not only the project series")
	fs.StringVar(&o.repo, "repo", "", "repo name to stamp on emitted records (default: the root directory name)")
	fs.StringVar(&o.commit, "commit", "", "commit to stamp on emitted records (default: git HEAD under --root)")
	fs.StringVar(&o.ts, "ts", "", "timestamp for emitted records, YYYY-MM-DD or RFC 3339 (default: now)")
	fs.StringVar(&o.org, "org", "",
		"organisation to attribute evidence to (else .quality.yaml org:, else inferred from git email; - for none)")
	fs.BoolVar(&o.strictCaps, "strict-caps", false, "mode=check: also fail if peak complexity exceeds the baseline")
	srcs := parseArgsMulti(fs, args)
	if len(srcs) == 0 {
		return errors.New(importUsage)
	}

	in, err := decodeImport(srcs, o)
	if err != nil {
		return err
	}
	if o.emit != "" {
		return emitImported(in, o)
	}
	switch o.mode {
	case "report":
		return importReport(in, o.asJSON)
	case "baseline":
		return importBaseline(in, o)
	case "check":
		return importCheck(in, o)
	}
	return fmt.Errorf("unknown mode %q (want report|baseline|check)", o.mode)
}

// importOpts holds the import flags.
type importOpts struct {
	root, format, tool, prefix, file, mode               string
	includeSuppressed, force, asJSON, detail, strictCaps bool
	emit, repo, commit, ts, org                          string
}

// imported is a decoded report, whichever producers wrote it.
type imported struct {
	rep    *model.Report
	cfg    verdict.Config
	sarifs int           // documents that were SARIF
	tools  []string      // producers the SARIF documents name, in order
	dcms   []*dcm.Result // documents that were DCM, merged into dcm once all are in
	dcm    *dcm.Result
}

// decodeImport reads and merges every document; a .NET solution writes one SARIF per project.
func decodeImport(srcs []string, o importOpts) (*imported, error) {
	cfg, err := analyze.LoadConfig(o.root)
	if err != nil {
		return nil, err
	}
	in := &imported{rep: &model.Report{Root: o.root}, cfg: cfg}
	for _, arg := range srcs {
		src := parseSource(arg, o.prefix)
		data, err := readInput(src.file)
		if err == nil {
			err = in.decode(data, o, src)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.file, err)
		}
	}
	if err := in.addDCM(); err != nil {
		return nil, err
	}
	if len(srcs) > 1 {
		fmt.Fprintf(os.Stderr, "merged %d %s\n", len(srcs), in.noun())
	}
	in.rep.Summarise(in.files())
	in.rep.Commit = o.commit
	if in.rep.Commit == "" {
		in.rep.Commit = gitCommit(o.root)
	}
	return in, nil
}

// decode reads one document by its format; a source's prefix applies to DCM paths.
func (in *imported) decode(data []byte, o importOpts, src source) error {
	switch pickFormat(o.format, data) {
	case "dcm":
		return in.decodeDCM(data, o, src.prefix)
	case "sarif":
		if src.explicit {
			return fmt.Errorf("=prefix applies to DCM reports only")
		}
		return in.decodeSARIF(data, o)
	}
	return fmt.Errorf("unknown --format %q (want auto|sarif|dcm)", o.format)
}

func (in *imported) decodeSARIF(data []byte, o importOpts) error {
	rep, err := sarif.ImportReport(bytes.NewReader(data),
		sarif.Options{Root: o.root, ToolPrefix: o.tool, IncludeSuppressed: o.includeSuppressed})
	if err != nil {
		return err
	}
	applyConfigVerdicts(rep.Findings, in.cfg)
	in.rep.Findings = append(in.rep.Findings, rep.Findings...)
	in.sarifs++
	for _, t := range rep.Tools {
		if !slices.Contains(in.tools, t) {
			in.tools = append(in.tools, t)
		}
	}
	return nil
}

// noun names what was merged.
func (in *imported) noun() string {
	if in.dcm == nil {
		return "SARIF files"
	}
	return "reports"
}

// files is the file count when a producer reported one; SARIF alone does not.
func (in *imported) files() int {
	if in.dcm != nil {
		return in.dcm.Files
	}
	return 0
}

// importReport prints findings by rule and, for DCM, what was measured.
func importReport(in *imported, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(in.rep)
	}
	if in.sarifs > 0 || in.dcm == nil {
		fmt.Printf("imported %d findings\n", len(in.rep.Findings))
		for _, k := range sortedCountKeys(in.rep.Summary.FindingsByRule) {
			fmt.Printf("  %-52s %d\n", k, in.rep.Summary.FindingsByRule[k])
		}
	}
	in.reportDCM()
	return nil
}

func baselinePath(o importOpts) string {
	if filepath.IsAbs(o.file) {
		return o.file
	}
	return filepath.Join(o.root, o.file)
}

func importBaseline(in *imported, o importOpts) error {
	path := baselinePath(o)
	if _, err := os.Stat(path); err == nil && !o.force {
		return fmt.Errorf("baseline already exists at %s\n"+
			"  regenerating it would silently forgive every current violation.\n"+
			"  use --force to overwrite", path)
	}
	b := baseline.From(in.rep, in.rep.Commit)
	if err := b.Save(path); err != nil {
		return err
	}
	fmt.Printf("wrote %s with %d tolerated findings\n", path, len(b.Tolerated))
	return nil
}

func importCheck(in *imported, o importOpts) error {
	if err := in.gateable(o); err != nil {
		return err
	}
	b, err := baseline.Load(baselinePath(o))
	if err != nil {
		return fmt.Errorf("%w\n  run `ratchet import --mode baseline` first", err)
	}
	res := b.Check(in.rep, o.strictCaps)
	fmt.Printf("%d tolerated, %d new, %d fixed\n", res.Existing, len(res.New), len(res.Fixed))
	if len(res.New) > 0 {
		fmt.Printf("\nNEW findings (these fail the build):\n")
		report.Findings(os.Stdout, res.New)
	}
	for _, cb := range res.CapBreak {
		fmt.Printf("\ncap breach: %s\n", cb)
	}
	if res.Regressed() {
		os.Exit(1)
	}
	return nil
}

// readInput reads the whole report so the format can be sniffed first.
func readInput(src string) ([]byte, error) {
	if src == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(src)
}

// applyConfigVerdicts gives imported findings the .quality.yaml treatment the native scan gets.
func applyConfigVerdicts(findings []model.Finding, cfg verdict.Config) {
	for i := range findings {
		f := &findings[i]
		if f.Verdict != "" {
			continue
		}
		if v, ok := cfg.Match(f.Rule, f.File); ok {
			verdict.Apply(f, v)
		}
	}
}

// emitImported writes an imported report as assay records.
func emitImported(in *imported, o importOpts) error {
	name := o.repo
	if name == "" {
		name = filepath.Base(mustAbs(o.root))
	}
	when, err := stampTime(o.ts)
	if err != nil {
		return err
	}
	switch o.emit {
	case "findings":
		return emitAssay(in.rep, "findings", name, in.rep.Commit, resolveOrg(o.org, in.cfg.Org, o.root), true, when, importedTool)
	case "measures":
		return emitMeasureRecords(in, o, name, when)
	}
	return fmt.Errorf("unknown --emit %q (want measures|findings)", o.emit)
}

// importedTool names a finding's producer: the SARIF driver its rule is namespaced by.
func importedTool(f model.Finding) string { return "sarif/" + producer(f.Rule) }

// producer is the tool a rule ID is namespaced by.
func producer(rule string) string {
	if i := strings.Index(rule, ":"); i > 0 {
		return rule[:i]
	}
	return "sarif"
}

// emitMeasureRecords writes the producer's measures, then the findings counts.
func emitMeasureRecords(in *imported, o importOpts, repo string, when time.Time) error {
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()
	put := func(m schema.Measure) error {
		m.Repo, m.Commit, m.TS = repo, in.rep.Commit, when
		return enc.Write(&m)
	}
	for _, m := range in.dcmMeasures(o.detail) {
		if err := put(m); err != nil {
			return err
		}
	}
	if in.sarifs == 0 && in.dcm != nil {
		return nil // DCM carries no findings source; a zero would blank a real lint series
	}
	return in.emitCounts(put, o.tool)
}

// emitCounts writes findings.<tool>.total per producer, so an import never overwrites the scan's
// own findings.total, and findings.rule.<rule>, which the rule's namespace already keeps apart.
func (in *imported) emitCounts(put func(schema.Measure) error, tool string) error {
	byTool := in.countByTool(tool)
	if byTool == nil {
		fmt.Fprintln(os.Stderr, "no findings to count and no run names a tool; pass --tool <name> to record a zero for that producer")
		return nil
	}
	proj := func(metric string, v float64) error {
		return put(schema.Measure{Scope: schema.ScopeProject, Metric: metric, Value: v})
	}
	for _, t := range sortedCountKeys(byTool) {
		if err := proj("findings."+t+".total", float64(byTool[t])); err != nil {
			return err
		}
	}
	for _, rule := range sortedCountKeys(in.rep.Summary.FindingsByRule) {
		if err := proj("findings.rule."+rule, float64(in.rep.Summary.FindingsByRule[rule])); err != nil {
			return err
		}
	}
	return nil
}

// countByTool counts findings per producer, seeded with a zero for every tool the documents
// name, so a clean run lands its zero. --tool names the producer when a document has no runs.
func (in *imported) countByTool(tool string) map[string]int {
	byTool := map[string]int{}
	for _, t := range in.tools {
		byTool[t] = 0
	}
	for _, f := range in.rep.Findings {
		byTool[producer(f.Rule)]++
	}
	if len(byTool) == 0 {
		if tool == "" {
			return nil
		}
		byTool[tool] = 0
	}
	return byTool
}

// stampTime parses --ts: empty is now, a bare date is midnight UTC, so a backfill lands on its day.
func stampTime(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--ts %q: want YYYY-MM-DD or RFC 3339", s)
	}
	return t.UTC(), nil
}
