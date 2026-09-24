package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

func ratchet(t *testing.T) cmdtest.Bin {
	t.Helper()
	return cmdtest.Build(t, "ratchet")
}

// ---------- the fixture ----------
//
// A small repository with a known set of violations, built in a temp directory
// so nothing outside the assay checkout can change the answer. Four findings,
// spread over two packages and three rules:
//
//	core/parse.go   naked-type-assertion, any-in-exported-signature, error-swallowed
//	plugin/hook.go  panic-in-library
//
// Nothing here is a real service; the names are deliberately generic.

const fixtureCore = `package core

import "errors"

// Decode is exported and takes any — an any-in-exported-signature warning.
func Decode(v any) string {
	return v.(string)
}

// Load checks an error and then returns nil, which discards it.
func Load(name string) error {
	err := open(name)
	if err != nil {
		return nil
	}
	return nil
}

func open(name string) error { return errors.New("cannot open " + name) }
`

const fixturePlugin = `package plugin

// Register panics rather than returning an error.
func Register(name string) {
	panic("not implemented: " + name)
}
`

// fixtureNewViolation adds exactly ONE finding, in a file the baseline has
// never seen. One, so "name only the new one" can be asserted exactly.
const fixtureNewViolation = `package core

// Box carries an untyped payload.
type Box struct{ Value any }

// Fetch asserts without the comma-ok form: one new violation, nothing else.
func Fetch(b *Box) string {
	return b.Value.(string)
}
`

const (
	fileCore   = "core/parse.go"
	filePlugin = "plugin/hook.go"
	fileNew    = "core/fetch.go"
)

func fixture(t *testing.T) string {
	t.Helper()
	return cmdtest.Tree(t, map[string]string{
		fileCore:   fixtureCore,
		filePlugin: fixturePlugin,
	})
}

// scanReport is the slice of `--format json` these tests assert on.
type scanReport struct {
	Summary struct {
		FindingsTotal int `json:"findingsTotal"`
	} `json:"summary"`
	Findings []struct {
		Rule        string `json:"rule"`
		File        string `json:"file"`
		Line        int    `json:"line"`
		Fingerprint string `json:"fingerprint"`
	} `json:"findings"`
}

func scanJSON(t *testing.T, bin cmdtest.Bin, dir string, args ...string) scanReport {
	t.Helper()
	r := bin.Run(t, dir, append([]string{"scan", ".", "--format", "json"}, args...)...).MustPass(t)
	var rep scanReport
	if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil {
		t.Fatalf("scan --format json did not emit parseable JSON: %v\n%s", err, r)
	}
	return rep
}

// ---------- 1. the ratchet contract ----------

// THE LOAD-BEARING TEST. Everything else this tool does is in service of this
// one sequence: record a dirty repository as tolerated, pass; add one new
// violation, fail, and name only the new one.
//
// If this breaks, the tool is either a gate nobody can adopt (it fails on day
// one debt) or a gate that never fires (it forgives regressions). There is no
// useful failure mode in between.
func TestRatchetToleratesTheOldAndFailsTheNew(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)

	before := scanJSON(t, bin, dir)
	if before.Summary.FindingsTotal == 0 {
		t.Fatal("the fixture produced no findings — a baseline over it would prove nothing")
	}

	base := bin.Run(t, dir, "baseline", ".").MustPass(t)
	base.MustSay(t, "tolerated findings")

	// A dirty repository, every violation recorded: the gate must pass. This is
	// the adoption story — you can turn it on today without fixing anything.
	bin.Run(t, dir, "check", ".").MustPass(t).
		MustSay(t, "0 new")

	cmdtest.WriteFile(t, dir, fileNew, fixtureNewViolation)

	after := scanJSON(t, bin, dir)
	if got, want := after.Summary.FindingsTotal, before.Summary.FindingsTotal+1; got != want {
		t.Fatalf("fixture drift: %d findings after adding one violation, want %d", got, want)
	}

	fail := bin.Run(t, dir, "check", ".").MustFail(t)
	fail.MustSay(t, "1 new", "NEW findings", filepath.ToSlash(fileNew))
	// Naming an already-tolerated file in the NEW section would make every
	// failure a hunt, which is how gates get switched off.
	fail.MustNotSay(t, filepath.ToSlash(filePlugin))
	if strings.Count(fail.Stdout, "core/parse.go") != 0 {
		t.Errorf("check named a tolerated file among the new findings:\n%s", fail)
	}

	// Removing the new violation restores the pass, so the failure was about
	// the change and not about accumulated state.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(fileNew))); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir, "check", ".").MustPass(t)
}

// Fixing a tolerated violation must be reported, not silently absorbed. A
// ratchet that only ever says "no regression" gives a team no reason to pay
// anything down.
func TestCheckReportsFixedFindings(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	bin.Run(t, dir, "baseline", ".").MustPass(t)

	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(filePlugin))); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir, "check", ".").MustPass(t).
		MustSay(t, "1 fixed", "ratchet baseline --tighten")
}

// ---------- 2. the baseline must not be silently regenerable ----------

// The safety property the whole mechanism rests on. If "the check failed" can
// be answered with "regenerate the baseline", the gate means nothing — so a
// rewrite has to be an explicit, reviewable act.
func TestBaselineCannotBeQuietlyRegenerated(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	bin.Run(t, dir, "baseline", ".").MustPass(t)

	original := cmdtest.ReadFile(t, dir, defaultBaselineFile)

	cmdtest.WriteFile(t, dir, fileNew, fixtureNewViolation)

	// A second baseline over a now-dirtier tree must refuse, and must say how
	// to do it deliberately.
	refused := bin.Run(t, dir, "baseline", ".").MustFail(t)
	refused.MustSay(t, "baseline already exists", "silently forgive", "--tighten", "--force")
	if got := cmdtest.ReadFile(t, dir, defaultBaselineFile); got != original {
		t.Error("the refused baseline command rewrote the file anyway")
	}

	// check is a reader. Failing or passing, it must not touch the file — a
	// gate that rewrites its own reference is a gate that always passes.
	bin.Run(t, dir, "check", ".").MustFail(t)
	if got := cmdtest.ReadFile(t, dir, defaultBaselineFile); got != original {
		t.Error("`check` rewrote the baseline")
	}
	bin.Run(t, dir, "check", ".", "--json").MustFail(t)
	if got := cmdtest.ReadFile(t, dir, defaultBaselineFile); got != original {
		t.Error("`check --json` rewrote the baseline")
	}

	// --force is the escape hatch and it does overwrite. That is the point:
	// the act is visible in the diff and in the command someone had to type.
	bin.Run(t, dir, "baseline", ".", "--force").MustPass(t)
	if got := cmdtest.ReadFile(t, dir, defaultBaselineFile); got == original {
		t.Error("--force did not overwrite the baseline")
	}
	bin.Run(t, dir, "check", ".").MustPass(t)
}

// --tighten drops what is genuinely fixed and keeps the rest, so paying debt
// down is a one-command ratchet in the other direction.
func TestBaselineTightenDropsOnlyWhatIsFixed(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	bin.Run(t, dir, "baseline", ".").MustPass(t)

	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(filePlugin))); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir, "baseline", ".", "--tighten").MustPass(t).
		MustSay(t, "removed 1 fixed entries")

	// The tightened baseline still tolerates everything that remains.
	bin.Run(t, dir, "check", ".").MustPass(t).MustSay(t, "0 new")

	// And the fixed violation can no longer come back for free.
	cmdtest.WriteFile(t, dir, filePlugin, fixturePlugin)
	bin.Run(t, dir, "check", ".").MustFail(t).MustSay(t, "1 new")
}

// A missing baseline is an error with instructions, not a silent pass. Exiting
// 0 when there is nothing to compare against is the quietest way for a gate to
// stop working: CI stays green and nobody looks again.
func TestCheckWithoutABaselineFails(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	bin.Run(t, dir, "check", ".").MustFail(t).
		MustSay(t, "run `ratchet baseline` first")
}

// ---------- 3. fingerprints exclude line numbers ----------

// Critical regression test. Fingerprints are keyed on file, rule, enclosing
// function and a whitespace-normalised snippet — deliberately NOT on the line.
// If a reformat moved them, gofmt on a large file would read as a wave of new
// violations and the gate would be switched off the same afternoon.
func TestFingerprintsSurviveReformatting(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)

	before := scanJSON(t, bin, dir)
	want := map[string]string{} // fingerprint -> rule@file
	for _, f := range before.Findings {
		want[f.Fingerprint] = f.Rule + "@" + f.File
	}
	if len(want) != len(before.Findings) {
		t.Fatalf("fingerprints collided within a single scan: %+v", before.Findings)
	}

	// Shift every line in the file: a licence header, blank lines between
	// declarations, and a blank line inside a function body for good measure.
	shifted := "// Copyright notice added later.\n//\n// Several lines of it.\n\n" +
		strings.ReplaceAll(fixtureCore, "\n\n", "\n\n\n")
	shifted = strings.Replace(shifted, "\terr := open(name)\n", "\terr := open(name)\n\n", 1)
	cmdtest.WriteFile(t, dir, fileCore, shifted)

	after := scanJSON(t, bin, dir)
	if len(after.Findings) != len(before.Findings) {
		t.Fatalf("reformatting changed the finding count: %d then %d",
			len(before.Findings), len(after.Findings))
	}

	movedLines := false
	for _, f := range after.Findings {
		if _, ok := want[f.Fingerprint]; !ok {
			t.Errorf("reformatting produced a NEW fingerprint for %s at %s:%d — "+
				"every gofmt would read as a regression", f.Rule, f.File, f.Line)
		}
		delete(want, f.Fingerprint)
	}
	for fp, what := range want {
		t.Errorf("fingerprint %s (%s) disappeared after a whitespace-only edit", fp, what)
	}
	for _, a := range after.Findings {
		for _, b := range before.Findings {
			if a.Rule == b.Rule && a.File == b.File && a.Line != b.Line {
				movedLines = true
			}
		}
	}
	if !movedLines {
		t.Error("no finding changed line, so this test did not actually exercise a shift")
	}

	// The end-to-end consequence: the baseline taken before the reformat still
	// covers the file after it.
	cmdtest.WriteFile(t, dir, fileCore, fixtureCore)
	bin.Run(t, dir, "baseline", ".").MustPass(t)
	cmdtest.WriteFile(t, dir, fileCore, shifted)
	bin.Run(t, dir, "check", ".").MustPass(t).MustSay(t, "0 new")
}

// The other half of the contract: a fingerprint has to actually change when the
// code does, or the baseline would tolerate a rewritten function forever.
func TestFingerprintChangesWhenTheCodeDoes(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	before := scanJSON(t, bin, dir)

	edited := strings.Replace(fixtureCore,
		"\treturn v.(string)\n", "\treturn v.(fmt.Stringer).String()\n", 1)
	edited = strings.Replace(edited, `import "errors"`, "import (\n\t\"errors\"\n\t\"fmt\"\n)", 1)
	cmdtest.WriteFile(t, dir, fileCore, edited)

	seen := map[string]bool{}
	for _, f := range before.Findings {
		seen[f.Fingerprint] = true
	}
	changed := false
	for _, f := range scanJSON(t, bin, dir).Findings {
		if f.Rule == "naked-type-assertion" && !seen[f.Fingerprint] {
			changed = true
		}
	}
	if !changed {
		t.Error("rewriting the asserted expression kept the same fingerprint — " +
			"the baseline would tolerate code nobody ever reviewed")
	}
}

// ---------- 4. exit codes ----------

// scan reports, it does not gate. Exiting non-zero on findings would make
// `ratchet scan` unusable as the measurement step of any pipeline.
func TestScanExitsZeroOnFindings(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	r := bin.Run(t, dir, "scan", ".").MustPass(t)
	r.MustSay(t, "findings")

	// --fail-on is the opt-in gate, and it respects severity rank. The fixture
	// carries error-level findings, so both error and info must trip it.
	bin.Run(t, dir, "scan", ".", "--fail-on", "error").MustFail(t)
	bin.Run(t, dir, "scan", ".", "--fail-on", "info").MustFail(t)

	clean := cmdtest.Tree(t, map[string]string{
		"tidy/tidy.go": "package tidy\n\nfunc Add(a, b int) int { return a + b }\n",
	})
	bin.Run(t, clean, "scan", ".").MustPass(t).MustSay(t, "no findings")
	bin.Run(t, clean, "scan", ".", "--fail-on", "error").MustPass(t)
}

// An unknown output format must be an error. Falling back to text would make a
// typo in a CI script look like a working pipeline producing empty JSON.
func TestScanRejectsUnknownFormat(t *testing.T) {
	bin := ratchet(t)
	bin.Run(t, fixture(t), "scan", ".", "--format", "yaml").MustFail(t).
		MustSay(t, "unknown format")
}

func TestUnknownCommandIsAnError(t *testing.T) {
	bin := ratchet(t)
	r := bin.Run(t, t.TempDir(), "sacn", ".").MustFail(t)
	r.MustSay(t, `unknown command "sacn"`, "usage:")
	if r.Code != 2 {
		t.Errorf("exit %d for an unknown command, want 2 (a usage error, not a findings failure)", r.Code)
	}
	// No arguments at all is the same class of mistake.
	if got := bin.Run(t, t.TempDir()).MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for no arguments, want 2", got)
	}
}

// ---------- 5. argument parsing ----------

// There was a real bug here: Go's flag package stops at the first non-flag
// argument, so `ratchet scan . --format json` silently emitted text. Everyone
// types the path first. Both orders must be the same command.
func TestFlagsWorkOnEitherSideOfThePath(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)

	pathFirst := bin.Run(t, dir, "scan", ".", "--skip-dirs", "plugin").MustPass(t)
	flagFirst := bin.Run(t, dir, "scan", "--skip-dirs", "plugin", ".").MustPass(t)
	if pathFirst.Stdout != flagFirst.Stdout {
		t.Errorf("flag position changed the result.\nwith path first:\n%s\nwith flag first:\n%s",
			pathFirst.Stdout, flagFirst.Stdout)
	}

	// Guard against a vacuous pass: --skip-dirs must have actually done
	// something, or both runs could be identical for the wrong reason.
	unskipped := bin.Run(t, dir, "scan", ".").MustPass(t)
	if unskipped.Stdout == pathFirst.Stdout {
		t.Fatal("--skip-dirs changed nothing, so this test proves nothing about flag position")
	}
	pathFirst.MustNotSay(t, "panic-in-library")
	unskipped.MustSay(t, "panic-in-library")

	// The same for a flag that changes the output format entirely — the exact
	// case the original bug was found on.
	jsonAfter := bin.Run(t, dir, "scan", ".", "--format", "json").MustPass(t)
	jsonBefore := bin.Run(t, dir, "scan", "--format", "json", ".").MustPass(t)
	if jsonAfter.Stdout != jsonBefore.Stdout {
		t.Error("--format is honoured on one side of the path and not the other")
	}
	if !strings.HasPrefix(strings.TrimSpace(jsonAfter.Stdout), "{") {
		t.Errorf("--format json after the path fell back to text:\n%s", jsonAfter.Stdout)
	}

	// And the ./... idiom people type by reflex resolves to the same thing.
	dots := bin.Run(t, dir, "scan", "./...", "--format", "json").MustPass(t)
	if dots.Stdout != jsonAfter.Stdout {
		t.Error("`scan ./...` and `scan .` disagree")
	}
}

// An unknown flag must stop the run. Silently ignoring one means a CI job that
// believes it is gating on something it is not.
func TestUnknownFlagIsAnError(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	for _, args := range [][]string{
		{"scan", ".", "--skip-dir", "plugin"}, // singular: a plausible typo
		{"scan", "--no-such-flag", "."},
		{"baseline", ".", "--wat"},
		{"check", ".", "--wat"},
	} {
		r := bin.Run(t, dir, args...).MustFail(t)
		r.MustSay(t, "flag provided but not defined")
	}
}

// SUSPECT: `ratchet scan a b` silently measures only `a`. parseArgs in main.go
// accumulates every positional and then returns positional[0], discarding the
// rest with no diagnostic — so a multi-directory invocation, or a shell glob
// that expanded to more paths than the author expected, reports on a subset and
// looks like a clean run. Asserting current behaviour; not fixed here.
func TestOnlyTheFirstPathIsScanned(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)

	both := bin.Run(t, dir, "scan", "core", "plugin", "--format", "json").MustPass(t)
	coreOnly := bin.Run(t, dir, "scan", "core", "--format", "json").MustPass(t)
	if both.Stdout != coreOnly.Stdout {
		t.Fatalf("behaviour changed: `scan core plugin` no longer equals `scan core`.\n"+
			"If extra positionals are now handled, delete this test and its SUSPECT note.\n%s", both)
	}
	if strings.Contains(both.Stdout, "panic-in-library") {
		t.Error("plugin/ was scanned after all — update this test")
	}
	if strings.Contains(both.Stderr, "plugin") {
		t.Error("the ignored positional is now reported; this test is stale")
	}
}

// ---------- 6. the JSONL contract ----------

// The emitted stream is the interface to every other tool in the set, so its
// shape is asserted here rather than only where it is consumed.
func TestEmitProducesOneRecordPerLine(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	rep := scanJSON(t, bin, dir)

	findings := bin.Run(t, dir, "scan", ".", "--emit", "findings",
		"--repo", "fixture", "--org", "-").MustPass(t)
	lines := cmdtest.Lines(findings.Stdout)
	if len(lines) != len(rep.Findings) {
		t.Fatalf("emitted %d finding lines for %d findings", len(lines), len(rep.Findings))
	}
	for i, l := range lines {
		var rec struct {
			V           int    `json:"v"`
			Kind        string `json:"kind"`
			Repo        string `json:"repo"`
			Rule        string `json:"rule"`
			Fingerprint string `json:"fingerprint"`
			Tool        string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i+1, err, l)
		}
		switch {
		case rec.Kind != "finding":
			t.Errorf("line %d has kind %q, want finding", i+1, rec.Kind)
		case rec.Repo != "fixture":
			t.Errorf("line %d was not stamped with --repo: %q", i+1, rec.Repo)
		case rec.Fingerprint == "":
			t.Errorf("line %d carries no fingerprint, so nothing downstream can key on it", i+1)
		case rec.Tool != "ratchet":
			t.Errorf("line %d does not name its producer: %q", i+1, rec.Tool)
		}
	}

	measures := bin.Run(t, dir, "scan", ".", "--emit", "measures",
		"--repo", "fixture", "--org", "-").MustPass(t)
	if len(cmdtest.Lines(measures.Stdout)) == 0 {
		t.Fatal("--emit measures produced nothing")
	}
	for _, l := range cmdtest.Lines(measures.Stdout) {
		var rec struct {
			Kind   string `json:"kind"`
			Metric string `json:"metric"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("measure line is not JSON: %v\n%s", err, l)
		}
		if rec.Kind != "measure" || rec.Metric == "" {
			t.Errorf("bad measure record: %s", l)
		}
	}

	bin.Run(t, dir, "scan", ".", "--emit", "nonsense").MustFail(t).
		MustSay(t, "unknown --emit")
}

// --org=- means "attribute this to nobody". Without it the tool falls back to
// the committer's email domain, and quietly stamping someone's employer on
// exported evidence is a surprise nobody wants to find after publishing.
func TestEmitWithNoOrgDoesNotGuess(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	cmdtest.WriteFile(t, dir, "core/judged.go", `package core

// quality:accepted the exported shape is frozen until v2
func Widen(v any) string {
	s, _ := v.(string)
	return s
}
`)
	orgs := func(flag string) []string {
		t.Helper()
		r := bin.Run(t, dir, "scan", ".", "--emit", "findings", "--repo", "fixture",
			"--org", flag).MustPass(t)
		var out []string
		for _, l := range cmdtest.Lines(r.Stdout) {
			var rec struct {
				Kind string `json:"kind"`
				Org  string `json:"org"`
			}
			_ = json.Unmarshal([]byte(l), &rec)
			if rec.Kind == "verdict" {
				out = append(out, rec.Org)
			}
		}
		// Without this the assertions below could pass by emitting nothing.
		if len(out) == 0 {
			t.Fatalf("an annotated finding produced no verdict record with --org %q", flag)
		}
		return out
	}

	for _, got := range orgs("acme") {
		if got != "acme" {
			t.Errorf("--org acme produced a verdict attributed to %q", got)
		}
	}
	for _, got := range orgs("-") {
		if got != "" {
			t.Errorf("--org=- still attributed the evidence to %q", got)
		}
	}
}

// ---------- the remaining subcommands ----------

// import is how the ratchet reaches languages ratchet cannot parse, so the
// SARIF path gets the same baseline/check treatment as the native one.
func TestImportSarifBaselineAndCheck(t *testing.T) {
	bin := ratchet(t)
	dir := t.TempDir()

	sarif := func(results string) string {
		return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"otherlint"}},"results":[` +
			results + `]}]}`
	}
	one := `{"ruleId":"NULLREF","level":"error","message":{"text":"possible nil dereference"},
		"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/Handler.cs"},
		"region":{"startLine":42,"startColumn":9}}}]}`
	two := `{"ruleId":"UNUSED","level":"warning","message":{"text":"unused local"},
		"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/Parser.cs"},
		"region":{"startLine":7,"startColumn":3}}}]}`

	cmdtest.WriteFile(t, dir, "first.sarif", sarif(one))
	bin.Run(t, dir, "import", "first.sarif", "--root", ".").MustPass(t).
		MustSay(t, "imported 1 findings", "NULLREF")

	bin.Run(t, dir, "import", "first.sarif", "--root", ".", "--mode", "baseline").MustPass(t).
		MustSay(t, "1 tolerated findings")
	bin.Run(t, dir, "import", "first.sarif", "--root", ".", "--mode", "check").MustPass(t).
		MustSay(t, "0 new")

	cmdtest.WriteFile(t, dir, "second.sarif", sarif(one+","+two))
	bin.Run(t, dir, "import", "second.sarif", "--root", ".", "--mode", "check").MustFail(t).
		MustSay(t, "1 new", "Parser.cs")

	// The same no-quiet-reset rule applies to an imported baseline.
	bin.Run(t, dir, "import", "second.sarif", "--root", ".", "--mode", "baseline").MustFail(t).
		MustSay(t, "silently forgive", "--force")

	bin.Run(t, dir, "import", "--root", ".").MustFail(t).MustSay(t, "usage: ratchet import")
	bin.Run(t, dir, "import", "first.sarif", "--mode", "sideways").MustFail(t).
		MustSay(t, "unknown mode")
}

func TestImportAppliesConfigVerdictsAndEmitsRecords(t *testing.T) {
	bin := ratchet(t)
	dir := cmdtest.Tree(t, map[string]string{
		".quality.yaml": "verdicts:\n  - rule: otherlint:NULLREF\n    verdict: wont-fix\n    reason: guarded by the caller\n",
		"r.sarif": `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"otherlint"}},"results":[
  {"ruleId":"NULLREF","level":"error","message":{"text":"possible nil dereference"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/Handler.cs"},"region":{"startLine":42,"startColumn":9}}}]},
  {"ruleId":"UNUSED","level":"warning","message":{"text":"unused local"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/Parser.cs"},"region":{"startLine":7,"startColumn":3}}}]}]}]}`,
		"clean.sarif":  `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"otherlint"}},"results":[]}]}`,
		"noruns.sarif": `{"version":"2.1.0","runs":[]}`,
		"not.json":     `{"records":[]}`,
	})

	bin.Run(t, dir, "import", "r.sarif", "--root", ".", "--json").MustPass(t).
		MustSay(t, `"verdict": "wont-fix"`, `"verdictSource": "config"`)

	f := bin.Run(t, dir, "import", "r.sarif", "--root", ".", "--emit", "findings",
		"--repo", "svc", "--commit", "abc123", "--ts", "2026-01-15", "--org=-").MustPass(t)
	f.MustSay(t, `"kind":"finding"`, `"tool":"sarif/otherlint"`, `"rule":"otherlint:UNUSED"`, `"repo":"svc"`,
		`"commit":"abc123"`, `"ts":"2026-01-15T00:00:00Z"`, `"kind":"verdict"`, `"verdict":"wont-fix"`)
	f.MustNotSay(t, `"kind":"measure"`, `"tool":"ratchet"`)

	// The counts are named per producer, so an import never overwrites the scan's own findings.total.
	m := bin.Run(t, dir, "import", "r.sarif", "--root", ".", "--emit", "measures", "--repo", "svc", "--org=-").MustPass(t)
	m.MustSay(t, `"kind":"measure"`, `"metric":"findings.otherlint.total","value":2`, `"metric":"findings.rule.otherlint:NULLREF"`)
	m.MustNotSay(t, `"metric":"findings.total"`)

	// A clean run records its zero: the document names the driver. With no runs at all there is
	// no producer to name unless --tool does.
	bin.Run(t, dir, "import", "clean.sarif", "--root", ".", "--emit", "measures", "--org=-").MustPass(t).
		MustSay(t, `"metric":"findings.otherlint.total","value":0`)
	z := bin.Run(t, dir, "import", "noruns.sarif", "--root", ".", "--emit", "measures", "--org=-").MustPass(t)
	z.MustSay(t, "pass --tool")
	z.MustNotSay(t, `"kind":"measure"`)
	bin.Run(t, dir, "import", "noruns.sarif", "--root", ".", "--emit", "measures", "--tool", "otherlint", "--org=-").MustPass(t).
		MustSay(t, `"metric":"findings.otherlint.total","value":0`)

	bin.Run(t, dir, "import", "r.sarif", "--root", ".", "--emit", "measures", "--ts", "yesterday").MustFail(t).
		MustSay(t, "want YYYY-MM-DD")
	bin.Run(t, dir, "import", "r.sarif", "--root", ".", "--emit", "sideways").MustFail(t).MustSay(t, "unknown --emit")
	bin.Run(t, dir, "import", "not.json", "--root", ".").MustFail(t).MustSay(t, "not a SARIF document")
}

// import validates .quality.yaml the way scan does: a bad verdict fails the import instead of being ignored.
func TestImportRejectsAnInvalidConfig(t *testing.T) {
	bin := ratchet(t)
	dir := cmdtest.Tree(t, map[string]string{
		".quality.yaml": "verdicts:\n  - rule: otherlint:NULLREF\n    verdict: wont-fix\n",
		"r.sarif":       `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"otherlint"}},"results":[]}]}`,
	})
	bin.Run(t, dir, "import", "r.sarif", "--root", ".").MustFail(t).MustSay(t, "needs a reason")
}

// exceptions is the "what have we agreed to live with" report. Its value is
// that an expired review-by date is visible; an exception nobody revisits is
// how a baseline becomes permanent.
func TestExceptionsListsJudgementsAndFlagsExpiry(t *testing.T) {
	bin := ratchet(t)
	dir := cmdtest.Tree(t, map[string]string{
		"core/judged.go": `package core

// quality:accepted until=2001-01-01 OLD-1 scheduled for the v2 rewrite
func Widen(v any) string {
	s, _ := v.(string)
	return s
}

// quality:wont-fix the shape is fixed by the wire format
func Passthrough(v any) any { return v }
`,
	})

	r := bin.Run(t, dir, "exceptions", ".").MustPass(t)
	r.MustSay(t, "accepted", "wont-fix", "2001-01-01", "past their review-by date")

	only := bin.Run(t, dir, "exceptions", ".", "--expired").MustPass(t)
	only.MustSay(t, "2001-01-01")
	only.MustNotSay(t, "wire format")

	bin.Run(t, dir, "exceptions", ".", "--json").MustPass(t).MustSay(t, `"Expired": true`)

	// A repository with nothing judged says so rather than printing an empty
	// table, because "no exceptions" and "the report is broken" look identical
	// otherwise.
	bin.Run(t, fixture(t), "exceptions", ".").MustPass(t).MustSay(t, "no exceptions recorded")
}

func TestRulesAndVersion(t *testing.T) {
	bin := ratchet(t)
	dir := t.TempDir()

	rules := bin.Run(t, dir, "rules").MustPass(t)
	for _, want := range []string{
		"naked-type-assertion", "error-swallowed", "any-in-exported-signature",
		"panic-in-library", "else-after-return",
	} {
		rules.MustSay(t, want)
	}
	bin.Run(t, dir, "version").MustPass(t).MustSay(t, "ratchet "+version)
	bin.Run(t, dir, "help").MustPass(t).MustSay(t, "hold the line against regression")
}

// learn refuses to draw a conclusion from a single repository, and says so.
// Two codebases disagreeing is the only thing that separates a convention from
// one team's habit.
func TestLearnWarnsOnASingleRepo(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	r := bin.Run(t, dir, "learn", dir).MustPass(t)
	r.MustSay(t, "one repository cannot distinguish a shared convention from local habit")
	bin.Run(t, dir, "learn").MustFail(t).MustSay(t, "usage: ratchet learn")
}

// ---------- the aggregate caps ----------

// fixtureComplex triggers no rule at all: it is just hard to read. That is the
// gap the caps exist to cover.
const fixtureComplex = `package core

// Churn is deeply nested and heavily branched, and violates no rule.
func Churn(xs []int, mode string) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			if mode == "double" {
				if x%2 == 0 {
					total += x * 2
				} else if x%3 == 0 {
					total += x * 3
				} else {
					total += x
				}
			} else if mode == "square" {
				for i := 0; i < x; i++ {
					if i%3 == 0 {
						total += i
					}
				}
			}
		}
	}
	return total
}
`

// The fingerprint set cannot see this one: a brand-new function that triggers
// no rule has no finding to compare, so an unmaintainable function could land
// without the gate noticing. The caps are the guard, and they are advisory
// unless asked for — gating on an aggregate invites gaming it.
func TestStrictCapsCatchWhatFingerprintsCannot(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	bin.Run(t, dir, "baseline", ".").MustPass(t)

	cmdtest.WriteFile(t, dir, "core/churn.go", fixtureComplex)

	// No new finding, so the default gate passes.
	bin.Run(t, dir, "check", ".").MustPass(t).MustSay(t, "0 new")

	// Asked to be strict, it reports the breach and fails.
	strict := bin.Run(t, dir, "check", ".", "--strict-caps").MustFail(t)
	strict.MustSay(t, "cap breach", "cognitive")
}

// ---------- output formats ----------

// Each format has a consumer: SARIF puts findings on the line in a pull
// request, CSV goes in a spreadsheet, JSON is the contract other tools read.
func TestScanRendersEveryFormat(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)

	csv := bin.Run(t, dir, "scan", ".", "--format", "csv").MustPass(t)
	if !strings.HasPrefix(csv.Stdout, "file,line,func,recv,exported,") {
		t.Errorf("csv is missing its header row:\n%s", csv.Stdout)
	}

	sarifOut := bin.Run(t, dir, "scan", ".", "--format", "sarif").MustPass(t)
	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string `json:"name"`
					Rules []struct {
						ID string `json:"id"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string `json:"level"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(sarifOut.Stdout), &doc); err != nil {
		t.Fatalf("sarif output is not JSON: %v\n%s", err, sarifOut.Stdout)
	}
	if doc.Version == "" || len(doc.Runs) != 1 || len(doc.Runs[0].Results) == 0 {
		t.Fatalf("sarif document is not usable by a code-scanning consumer: %+v", doc)
	}
	// A result whose ruleId is not declared in the driver is dropped silently
	// by GitHub, which would look like the scan found nothing.
	declared := map[string]bool{}
	for _, r := range doc.Runs[0].Tool.Driver.Rules {
		declared[r.ID] = true
	}
	for _, r := range doc.Runs[0].Results {
		if !declared[r.RuleID] {
			t.Errorf("result for undeclared rule %q", r.RuleID)
		}
	}
}

// ---------- history ----------

// history is the one command that genuinely needs git: per-function complexity
// needs a parsed tree, which a diff cannot give. It archives each sampled
// commit into a temp directory rather than touching the working tree, and this
// asserts that — a tool that checked out over someone's uncommitted work would
// be unusable on a machine anyone is working on.
func TestHistoryReplaysOverCommits(t *testing.T) {
	bin := ratchet(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("no git on PATH (%v) — history cannot be exercised", err)
	}
	dir := fixture(t)
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), cmdtest.GitEnv()...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git %v failed (%v): %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	cmdtest.WriteFile(t, dir, fileNew, fixtureNewViolation)
	git("add", "-A")
	git("commit", "-q", "-m", "second")

	r := bin.Run(t, dir, "history", ".", "--interval", "all").MustPass(t)
	r.MustSay(t, "sampling 2 commits", "emitted 2 records, skipped 0")

	lines := cmdtest.Lines(r.Stdout)
	if len(lines) != 2 {
		t.Fatalf("history emitted %d records for 2 commits:\n%s", len(lines), r)
	}
	var totals []int
	for i, l := range lines {
		var rep struct {
			Commit  string `json:"commit"`
			Root    string `json:"root"`
			Summary struct {
				FindingsTotal int `json:"findingsTotal"`
			} `json:"summary"`
		}
		if err := json.Unmarshal([]byte(l), &rep); err != nil {
			t.Fatalf("history record %d is not JSON: %v\n%s", i, err, l)
		}
		if rep.Commit == "" {
			t.Errorf("record %d carries no commit, so the series cannot be placed in time", i)
		}
		// The temp checkout path must not leak into the series: it differs on
		// every run and would make two identical histories look different.
		if rep.Root != "" {
			t.Errorf("record %d leaked a scratch path as its root: %q", i, rep.Root)
		}
		totals = append(totals, rep.Summary.FindingsTotal)
	}
	// Oldest first: a series that arrives backwards is useless as a trend.
	if totals[0] >= totals[1] {
		t.Errorf("series is not oldest-first — findings went %d then %d, "+
			"but the second commit added a violation", totals[0], totals[1])
	}

	// --max caps the sample, and the working tree is untouched throughout.
	bin.Run(t, dir, "history", ".", "--interval", "all", "--max", "1").MustPass(t).
		MustSay(t, "sampling 1 commits")
	st := exec.Command("git", "-C", dir, "status", "--porcelain")
	st.Env = append(os.Environ(), cmdtest.GitEnv()...)
	if out, err := st.Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("history dirtied the working tree: %q (err %v)", out, err)
	}
}

// A rule the caller switched off must not reach the baseline, or --rules would
// be a filter on the report only and the gate would fire on hidden findings.
func TestRulesFlagNarrowsTheScan(t *testing.T) {
	bin := ratchet(t)
	dir := fixture(t)
	rep := scanJSON(t, bin, dir, "--rules", "naked-type-assertion")
	if len(rep.Findings) == 0 {
		t.Fatal("--rules naked-type-assertion produced nothing")
	}
	for _, f := range rep.Findings {
		if f.Rule != "naked-type-assertion" {
			t.Errorf("rule %q fired while only naked-type-assertion was enabled", f.Rule)
		}
	}
}

// REGRESSION. `ratchet import a.sarif b.sarif` used to take the first file and
// discard the rest with no diagnostic.
//
// That is not a corner case for C#: a .NET solution writes one SARIF per
// project, because a single shared ErrorLog path has each project overwrite the
// last. Importing one file at a time gives you a baseline covering a fraction
// of the codebase and a green check over the rest.
func TestImportMergesSeveralSarifFiles(t *testing.T) {
	bin := ratchet(t)
	dir := t.TempDir()

	one := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Roslyn"}},"results":[
      {"ruleId":"CA1001","level":"warning","message":{"text":"a"},"locations":[{"physicalLocation":
      {"artifactLocation":{"uri":"P1/A.cs"},"region":{"startLine":3}}}]}]}]}`
	two := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Roslyn"}},"results":[
      {"ruleId":"CA2000","level":"warning","message":{"text":"b"},"locations":[{"physicalLocation":
      {"artifactLocation":{"uri":"P2/B.cs"},"region":{"startLine":7}}}]},
      {"ruleId":"CA2001","level":"warning","message":{"text":"c"},"locations":[{"physicalLocation":
      {"artifactLocation":{"uri":"P2/C.cs"},"region":{"startLine":9}}}]}]}]}`
	cmdtest.WriteFile(t, dir, "p1.sarif", one)
	cmdtest.WriteFile(t, dir, "p2.sarif", two)

	r := bin.Run(t, dir, "import", "p1.sarif", "p2.sarif", "--mode", "report").MustPass(t)
	r.MustSay(t, "merged 2 SARIF files", "imported 3 findings", "CA1001", "CA2000", "CA2001")

	// And the baseline must cover all of them, or the gate protects one project.
	bin.Run(t, dir, "import", "p1.sarif", "p2.sarif", "--mode", "baseline",
		"--file", "b.json", "--force").MustPass(t).MustSay(t, "3 tolerated")
	bin.Run(t, dir, "import", "p1.sarif", "p2.sarif", "--mode", "check",
		"--file", "b.json").MustPass(t).MustSay(t, "3 tolerated, 0 new")
}

// A solution build analyses a shared project once per referencing .csproj, so
// the same diagnostic arrives in several SARIF files. The baseline is keyed by
// fingerprint, so it must collapse them — otherwise the tolerated count grows
// with the number of projects that happen to reference a library.
func TestMergedDuplicatesCollapseInTheBaseline(t *testing.T) {
	bin := ratchet(t)
	dir := t.TempDir()
	same := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Roslyn"}},"results":[
      {"ruleId":"CA1001","level":"warning","message":{"text":"a"},"locations":[{"physicalLocation":
      {"artifactLocation":{"uri":"Shared/A.cs"},"region":{"startLine":3}}}]}]}]}`
	cmdtest.WriteFile(t, dir, "a.sarif", same)
	cmdtest.WriteFile(t, dir, "b.sarif", same)

	bin.Run(t, dir, "import", "a.sarif", "b.sarif", "--mode", "baseline",
		"--file", "b.json", "--force").MustPass(t).MustSay(t, "1 tolerated")
	bin.Run(t, dir, "import", "a.sarif", "b.sarif", "--mode", "check",
		"--file", "b.json").MustPass(t).MustSay(t, "0 new")
}
