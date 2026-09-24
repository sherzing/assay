package sarif

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/model"
)

func imp(t *testing.T, js string, opt Options) []model.Finding {
	t.Helper()
	fs, err := Import(strings.NewReader(js), opt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return fs
}

const basic = `{"version":"2.1.0","runs":[{
 "tool":{"driver":{"name":"testlint","rules":[
   {"id":"R1","shortDescription":{"text":"rule one"},"defaultConfiguration":{"level":"error"}}]}},
 "results":[
  {"ruleId":"R1","message":{"text":"boom"},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/a.go"},
     "region":{"startLine":10,"startColumn":3,"snippet":{"text":"x := y.(int)"}}}}]}]}]}`

func TestImportBasic(t *testing.T) {
	fs := imp(t, basic, Options{})
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs))
	}
	f := fs[0]
	if f.Rule != "testlint:R1" {
		t.Errorf("rule = %q, want testlint:R1 (tool-namespaced)", f.Rule)
	}
	if f.File != "src/a.go" || f.Line != 10 || f.Col != 3 {
		t.Errorf("location = %s:%d:%d, want src/a.go:10:3", f.File, f.Line, f.Col)
	}
	if f.Severity != model.Error {
		t.Errorf("severity = %q, want error (from rule defaultConfiguration)", f.Severity)
	}
	if f.Fingerprint == "" {
		t.Error("no fingerprint generated")
	}
}

// Two tools emitting the same rule ID must not collide in one baseline, or each
// silently tolerates the other's violations.
func TestRuleIDsAreNamespacedByTool(t *testing.T) {
	mk := func(tool string) string {
		return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"` + tool + `"}},
		 "results":[{"ruleId":"unused","message":{"text":"m"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	}
	a := imp(t, mk("golangci-lint"), Options{})
	b := imp(t, mk("roslyn"), Options{})
	if a[0].Rule == b[0].Rule {
		t.Fatalf("rules collided across tools: both %q", a[0].Rule)
	}
	if a[0].Fingerprint == b[0].Fingerprint {
		t.Error("fingerprints collided across tools — one tool's baseline would excuse the other's findings")
	}
}

// The same reason native findings exclude the line number: a reformat must not
// read as a wave of new violations.
func TestFingerprintSurvivesLineMovement(t *testing.T) {
	mk := func(line int) string {
		return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
		 "results":[{"ruleId":"R","message":{"text":"same defect"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},
		   "region":{"startLine":` + itoa(line) + `,"snippet":{"text":"foo(bar)"}}}}]}]}]}`
	}
	a := imp(t, mk(10), Options{})
	b := imp(t, mk(90), Options{})
	if a[0].Line == b[0].Line {
		t.Fatal("test not exercising line movement")
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("fingerprint changed on line movement alone: %s vs %s",
			a[0].Fingerprint, b[0].Fingerprint)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A producer that supplies its own stable fingerprint knows better than we do.
func TestProducerFingerprintPreferred(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"codeql"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "partialFingerprints":{"primaryLocationLineHash":"deadbeefcafe0123456789"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	f := imp(t, js, Options{})[0]
	if f.Fingerprint != "deadbeefcafe0123" {
		t.Errorf("fingerprint = %q, want the producer's primaryLocationLineHash", f.Fingerprint)
	}
}

// Honouring suppressions is the same principle as honouring //nolint: a tool
// that cannot be told no gets switched off entirely.
func TestSuppressionsHonoured(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "suppressions":[{"kind":"inSource","justification":"reviewed"}],
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	if got := len(imp(t, js, Options{})); got != 0 {
		t.Errorf("suppressed result imported by default: got %d, want 0", got)
	}
	if got := len(imp(t, js, Options{IncludeSuppressed: true})); got != 1 {
		t.Errorf("--include-suppressed did not import it: got %d, want 1", got)
	}
}

func TestAbsolutePathsMadeRelative(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///repo/src/a.cs"},"region":{"startLine":1}}}]}]}]}`
	f := imp(t, js, Options{Root: "/repo"})[0]
	if f.File != "src/a.cs" {
		t.Errorf("file = %q, want src/a.cs — imported findings must key like native ones", f.File)
	}
}

func TestSeverityMapping(t *testing.T) {
	cases := map[string]model.Severity{
		"error": model.Error, "warning": model.Warn,
		"note": model.Info, "none": model.Info,
		"": model.Warn, // SARIF's documented default
	}
	for level, want := range cases {
		js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
		 "results":[{"ruleId":"R","level":"` + level + `","message":{"text":"m"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
		if got := imp(t, js, Options{})[0].Severity; got != want {
			t.Errorf("level %q -> %q, want %q", level, got, want)
		}
	}
}

// A finding nobody can locate is not actionable, and cannot be fingerprinted
// stably. Dropping it beats carrying it.
func TestResultWithoutLocationDropped(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"no location"}}]}]}`
	if got := len(imp(t, js, Options{})); got != 0 {
		t.Errorf("got %d findings, want 0", got)
	}
}

func TestLogicalLocationBecomesFunc(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.cs"},"region":{"startLine":1}},
	   "logicalLocations":[{"name":"Get","fullyQualifiedName":"Api.Controller.Get","kind":"function"}]}]}]}]}`
	if got := imp(t, js, Options{})[0].Func; got != "Api.Controller.Get" {
		t.Errorf("func = %q, want the fully qualified logical location", got)
	}
}

func TestEmptyAndMalformed(t *testing.T) {
	if fs := imp(t, `{"version":"2.1.0","runs":[]}`, Options{}); len(fs) != 0 {
		t.Errorf("empty runs gave %d findings", len(fs))
	}
	if _, err := Import(strings.NewReader("not json"), Options{}); err == nil {
		t.Error("malformed SARIF should error, not silently produce nothing")
	}
}

// REGRESSION. SARIF 1.0 nests results under a different shape, so this decoder
// found nothing in one and reported "imported 0 findings" — a clean bill of
// health for a file it could not read.
//
// Roslyn's `-p:ErrorLog=out.sarif` emits 1.0 BY DEFAULT, so the obvious way to
// get diagnostics out of a .NET build produced exactly the file that was
// silently swallowed. Getting 2.1 requires `ErrorLog=out.sarif,version=2.1`.
func TestUnsupportedSarifVersionIsRefused(t *testing.T) {
	v1 := `{"version":"1.0.0","runs":[{"results":[{"ruleId":"CA1822"}]}]}`
	_, err := Import(strings.NewReader(v1), Options{})
	if err == nil {
		t.Fatal("SARIF 1.0 was accepted; it decodes to zero findings and reads as a clean scan")
	}
	for _, want := range []string{"1.0.0", "2.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the fix is obvious: %v", want, err)
		}
	}

	// 2.1.0 still works, and a producer that omits the version is not rejected
	// — plenty of tools emit conforming documents without stating it.
	ok := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"T"}},"results":[` +
		`{"ruleId":"R","message":{"text":"m"},"locations":[{"physicalLocation":` +
		`{"artifactLocation":{"uri":"a.cs"},"region":{"startLine":1}}}]}]}]}`
	if f, err := Import(strings.NewReader(ok), Options{}); err != nil || len(f) != 1 {
		t.Errorf("2.1.0: got %d findings, err %v", len(f), err)
	}
	noVer := strings.Replace(ok, `"version":"2.1.0",`, "", 1)
	if _, err := Import(strings.NewReader(noVer), Options{}); err != nil {
		t.Errorf("a document with no version field was rejected: %v", err)
	}
}

// Fixtures captured from a REAL `dotnet build` on a .NET 8 service, one with
// each ErrorLog spelling. Written from the spec these tests passed while the
// product was broken; written from the output they pin what Roslyn actually
// emits.
func TestRealRoslynOutput(t *testing.T) {
	t.Run("2.1 imports", func(t *testing.T) {
		f, err := ImportFile("testdata/roslyn-v21.sarif", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(f) != 2 {
			t.Fatalf("got %d findings, want 2", len(f))
		}
		if f[0].Rule == "" || f[0].File == "" || f[0].Line == 0 || f[0].Message == "" {
			t.Errorf("finding is missing a field a person needs: %+v", f[0])
		}
	})

	// The version probe has to run BEFORE the full decode. SARIF 1.0 carries
	// `message` as a string where 2.1 has an object, so decoding first fails
	// with "cannot unmarshal string into Go struct field ... Text string" —
	// true about Go, useless to someone holding a build log.
	t.Run("1.0 is diagnosed, not type-errored", func(t *testing.T) {
		_, err := ImportFile("testdata/roslyn-v1.sarif", Options{})
		if err == nil {
			t.Fatal("real Roslyn 1.0 output was accepted")
		}
		if strings.Contains(err.Error(), "unmarshal") {
			t.Errorf("leaked a Go decoding error instead of the diagnosis: %v", err)
		}
		for _, want := range []string{"1.0.0", "%2c"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
	})
}

// A document with neither a version nor runs is not SARIF at all. Reading it as SARIF used to
// yield "imported 0 findings", a clean bill of health for a file that was never read.
func TestNonSarifJSONIsRefused(t *testing.T) {
	for _, doc := range []string{`{}`, `{"formatVersion":13,"metricResults":[]}`, `{"records":[]}`} {
		_, err := Import(strings.NewReader(doc), Options{})
		if err == nil || !strings.Contains(err.Error(), "not a SARIF document") {
			t.Errorf("%s: err = %v, want a refusal", doc, err)
		}
	}
	if fs, err := Import(strings.NewReader(`{"version":"2.1.0"}`), Options{}); err != nil || len(fs) != 0 {
		t.Errorf("a versioned document with no runs is empty, not wrong: %v, %v", fs, err)
	}
}

// A run with no results still names its tool, which is what lets an import record a zero for it.
func TestReportListsTheToolsThatRan(t *testing.T) {
	two := `{"version":"2.1.0","runs":[
	  {"tool":{"driver":{"name":"otherlint"}},"results":[]},
	  {"tool":{"driver":{"name":"otherlint"}},"results":[]},
	  {"tool":{"driver":{"name":"roslyn"}},"results":[]}]}`
	rep, err := ImportReport(strings.NewReader(two), Options{})
	if err != nil || len(rep.Findings) != 0 || strings.Join(rep.Tools, ",") != "otherlint,roslyn" {
		t.Errorf("tools = %v, findings = %d, err = %v; want otherlint,roslyn once each", rep.Tools, len(rep.Findings), err)
	}
	if rep, _ := ImportReport(strings.NewReader(two), Options{ToolPrefix: "lint"}); strings.Join(rep.Tools, ",") != "lint" {
		t.Errorf("with ToolPrefix tools = %v, want lint", rep.Tools)
	}
	if rep, _ := ImportReport(strings.NewReader(`{"version":"2.1.0","runs":[]}`), Options{}); len(rep.Tools) != 0 {
		t.Errorf("no runs, yet tools = %v", rep.Tools)
	}
	if rep, _ := ImportReport(strings.NewReader(basic), Options{}); len(rep.Findings) != 1 || rep.Tools[0] != "testlint" {
		t.Errorf("basic: %+v", rep)
	}
}
