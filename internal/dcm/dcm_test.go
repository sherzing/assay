package dcm

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sherzing/assay/pkg/schema"
)

// report is a DCM document in the documented shape, with the declarations DCM names identically:
// a getter/setter pair, a local function, two unnamed extensions, and bodiless members.
const report = `{"formatVersion":13,"timestamp":"2026-09-22 10:00:00",
"analyzeResults":[{"path":"lib/a.dart","issues":[
 {"id":"avoid-dynamic","message":"Avoid using dynamic type.","location":{"startLine":5,"startColumn":3,"endLine":5,"endColumn":10},"severity":"warning"},
 {"id":"prefer-returning-conditional","message":"Prefer returning the result directly.","location":{"startLine":8,"startColumn":5,"endLine":8,"endColumn":18},"severity":"style"}]}],
"metricResults":[{"path":"lib/a.dart","issues":[
 {"id":"number-of-methods","location":{"startLine":3,"endLine":31},"level":"none","threshold":10,"value":6,"declarationName":"Box","declarationType":"class"},
 {"id":"weight-of-class","location":{"startLine":3,"endLine":31},"level":"none","threshold":0.33,"value":0.5,"declarationName":"Box","declarationType":"class"},
 {"id":"lines-of-code","location":{"startLine":4,"endLine":4},"level":"none","threshold":100,"value":1,"declarationName":"Box.Box","declarationType":"constructor"},
 {"id":"cyclomatic-complexity","location":{"startLine":7,"endLine":7},"level":"none","threshold":20,"value":4,"declarationName":"Box.value","declarationType":"getter"},
 {"id":"cyclomatic-complexity","location":{"startLine":8,"endLine":16},"level":"none","threshold":20,"value":1,"declarationName":"Box.value","declarationType":"setter"},
 {"id":"maximum-nesting-level","location":{"startLine":8,"endLine":16},"level":"none","threshold":5,"value":2,"declarationName":"Box.value","declarationType":"setter"},
 {"id":"cyclomatic-complexity","location":{"startLine":18,"endLine":26},"level":"none","threshold":20,"value":3,"declarationName":"Box.work","declarationType":"method"},
 {"id":"source-lines-of-code","location":{"startLine":18,"endLine":26},"level":"none","threshold":50,"value":9,"declarationName":"Box.work","declarationType":"method"},
 {"id":"some-new-metric","location":{"startLine":18,"endLine":26},"level":"none","threshold":1,"value":"2.5","declarationName":"Box.work","declarationType":"method"},
 {"id":"cyclomatic-complexity","location":{"startLine":19,"endLine":22},"level":"none","threshold":20,"value":2,"declarationName":"inner","declarationType":"function"},
 {"id":"cyclomatic-complexity","message":"This method has a cyclomatic complexity of 25, which exceeds the maximum of 20 allowed.","location":{"startLine":27,"endLine":29},"level":"very high","threshold":20,"value":25,"declarationName":"Box.sub","declarationType":"method"},
 {"id":"number-of-parameters","location":{"startLine":30,"endLine":30},"level":"none","threshold":4,"value":0,"declarationName":"Box.clear","declarationType":"method"},
 {"id":"number-of-methods","location":{"startLine":33,"endLine":35},"level":"none","threshold":10,"value":1,"declarationName":"Unnamed","declarationType":"extension"},
 {"id":"cyclomatic-complexity","location":{"startLine":34,"endLine":34},"level":"none","threshold":20,"value":1,"declarationName":"triple","declarationType":"method"},
 {"id":"number-of-methods","location":{"startLine":37,"endLine":39},"level":"none","threshold":10,"value":1,"declarationName":"Unnamed","declarationType":"extension"},
 {"id":"cyclomatic-complexity","location":{"startLine":38,"endLine":38},"level":"none","threshold":20,"value":1,"declarationName":"triple","declarationType":"method"},
 {"id":"cyclomatic-complexity","location":{"startLine":41,"endLine":46},"level":"none","threshold":20,"value":2,"declarationName":"topLevel","declarationType":"function"},
 {"id":"number-of-imports","location":{"startLine":1,"endLine":1},"level":"none","threshold":10,"value":1}]}],
"unusedFilesResults":[{"path":"lib/old.dart","issues":[{"id":"unused-file-issue","message":"Unused file","effortInMinutes":5}]}],
"unusedCodeResults":[{"path":"lib/a.dart","issues":[{"id":"unused-code-issue","message":"Unused method sub","location":{"startLine":27,"startColumn":7,"endLine":27,"endColumn":10},"declarationName":"sub","declarationType":"method"}]}],
"duplicationResults":[{"path":"lib/a.dart","issues":[{"id":"duplication-issue","message":"This method has 1 duplicate declaration","location":{"startLine":18,"startColumn":3,"endLine":26,"endColumn":4},"declarationName":"work","declarationType":"method","duplications":[{"declarationName":"plus","declarationType":"method","location":{"startLine":20,"startColumn":3,"endLine":22,"endColumn":4},"relativePath":"lib/b.dart"}]}]}],
"summary":[{"title":"Scanned files","value":1}]}`

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func imp(t *testing.T, doc string, opt Options) *Result {
	t.Helper()
	res, err := Import(strings.NewReader(doc), opt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

func has(res *Result, scope schema.Scope, path, metric string) bool {
	for _, m := range res.Measures {
		if m.Scope == scope && m.Path == path && m.Metric == metric {
			return true
		}
	}
	return false
}

func measure(t *testing.T, res *Result, scope schema.Scope, path, metric string) float64 {
	t.Helper()
	for _, m := range res.Measures {
		if m.Scope == scope && m.Path == path && m.Metric == metric {
			return m.Value
		}
	}
	t.Fatalf("no measure %s %q %s", scope, path, metric)
	return 0
}

func TestSniffAndFormatVersion(t *testing.T) {
	if !Sniff([]byte(report)) {
		t.Error("a DCM report was not recognised")
	}
	if Sniff([]byte(`{"version":"2.1.0","runs":[]}`)) {
		t.Error("a SARIF document was mistaken for DCM")
	}
	if _, err := Import(strings.NewReader(`{"version":"2.1.0","runs":[]}`), Options{}); err == nil ||
		!strings.Contains(err.Error(), "formatVersion") {
		t.Errorf("SARIF fed to the DCM importer: err = %v, want a formatVersion complaint", err)
	}
	if _, err := Import(strings.NewReader(`not json`), Options{}); err == nil {
		t.Error("garbage imported without error")
	}
	old := `{"formatVersion":11,"metricResults":[{"path":"lib/x.dart","issues":[
	  {"id":"cyclomatic-complexity","location":{"startLine":2},"value":4,"declarationName":"f","declarationType":"function"}]}]}`
	if res := imp(t, old, Options{}); res.FormatVersion != 11 {
		t.Errorf("formatVersion = %d, want 11 reported for the caller to warn about", res.FormatVersion)
	}
}

// DCM measures only what analysis_options.yaml configures, so a report with no metric values
// is a missing config, not an empty repository; importing it would store zeros as if it were.
func TestNoMetricValuesIsAnError(t *testing.T) {
	for _, doc := range []string{
		`{"formatVersion":13}`,
		`{"formatVersion":13,"metricResults":[]}`,
		`{"formatVersion":13,"metricResults":[{"path":"lib/a.dart","issues":[]}]}`,
		`{"formatVersion":2,"records":[{"path":"lib/a.dart","cyclomatic":9}]}`,
	} {
		_, err := Import(strings.NewReader(doc), Options{})
		if err == nil || !strings.Contains(err.Error(), "dcm: metrics:") {
			t.Errorf("%s: err = %v, want the analysis_options hint", doc, err)
		}
	}
}

func TestIssuesAsSingleObjectIsAccepted(t *testing.T) {
	res := imp(t, `{"formatVersion":13,
	  "metricResults":[{"path":"lib/x.dart","issues":{"id":"cyclomatic-complexity","location":{"startLine":2},"value":4,"declarationName":"f"}}]}`, Options{})
	if v := measure(t, res, schema.ScopeFunction, "lib/x.dart:f", "cyclomatic"); v != 4 {
		t.Errorf("object-shaped issues = %v", v)
	}
}

var update = flag.Bool("update", false, "rewrite testdata/demo/golden.json from the current importer output")

func TestRealReportGolden(t *testing.T) {
	root := filepath.Join("testdata", "demo")
	res, err := ImportFile(filepath.Join(root, "report.json"), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, res, filepath.Join(root, "golden.json"))
	checkFixtureFacts(t, res)
}

// checkGolden pins the importer's output byte for byte; -update rewrites it.
func checkGolden(t *testing.T, res *Result, golden string) {
	t.Helper()
	got, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("importer output for the real report changed: diff %s against a run with -update, then decide whether the change is intended", golden)
	}
}

// checkFixtureFacts says in words what the golden pins in numbers.
func checkFixtureFacts(t *testing.T, res *Result) {
	t.Helper()
	if res.FormatVersion != Version {
		t.Errorf("fixture is formatVersion %d, importer pinned at %d", res.FormatVersion, Version)
	}
	if res.Generated != 3 || res.Located == 0 || res.Issues == 0 {
		t.Errorf("generated = %d, located = %d, issues = %d; the fixture carries three generated files, sources on disk and lint issues",
			res.Generated, res.Located, res.Issues)
	}
	for _, want := range []string{"lib/box.dart:Box.value", "lib/box.dart:Box.value=", "lib/box.dart:Box.work.inner", "lib/box.dart:Unnamed.triple", "lib/box.dart:Box.fallback"} {
		if !has(res, schema.ScopeFunction, want, "cyclomatic") {
			t.Errorf("no cyclomatic measure for %s", want)
		}
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/box.dart:Box.fallback", "nesting"); v != 0 {
		t.Errorf("an initialiser-list constructor has nesting %v, want 0", v)
	}
	seen := map[string]bool{}
	for _, m := range res.Measures {
		if m.Scope == schema.ScopeProject {
			t.Errorf("project-scope %s in the result; roll-ups are the caller's", m.Metric)
		}
		key := string(m.Scope) + " " + m.Path + " " + m.Metric
		if seen[key] {
			t.Errorf("duplicate measure %s", key)
		}
		seen[key] = true
	}
}
