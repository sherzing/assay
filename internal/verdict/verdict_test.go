package verdict

import (
	"testing"
	"time"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

func TestParseAllThreeVerdicts(t *testing.T) {
	cases := map[string]schema.Judgement{
		"// quality:false-positive the SQL driver idiom": schema.FalsePositive,
		"// quality:accepted DEBT-412 needs v2":          schema.Accepted,
		"// quality:wont-fix panics by design":           schema.WontFix,
	}
	for text, want := range cases {
		v, ok := Parse(text)
		if !ok {
			t.Fatalf("did not parse: %q", text)
		}
		if v.Verdict != want {
			t.Errorf("%q → %q, want %q", text, v.Verdict, want)
		}
		if v.Reason == "" {
			t.Errorf("%q lost its reason", text)
		}
	}
}

// The marker is deliberately not tool-branded, so another analyser can honour
// the same decision. If someone "helpfully" renames it to assay:, this fails.
func TestMarkerIsToolNeutral(t *testing.T) {
	if Marker != "quality:" {
		t.Fatalf("marker is %q — a tool-branded prefix makes every other tool ignore it", Marker)
	}
	if _, ok := Parse("// assay:false-positive nope"); ok {
		t.Error("a tool-branded prefix should not be the supported form")
	}
}

func TestParseIgnoresUnrelatedComments(t *testing.T) {
	for _, s := range []string{
		"// this is a false-positive in the old system",
		"// TODO: accepted by the team",
		"// nolint:errcheck",
		"",
	} {
		if _, ok := Parse(s); ok {
			t.Errorf("wrongly parsed prose as a verdict: %q", s)
		}
	}
}

func TestUntilIsParsedAndStripped(t *testing.T) {
	v, ok := Parse("// quality:accepted until=2026-12-31 DEBT-412 needs the v2 migration")
	if !ok {
		t.Fatal("did not parse")
	}
	if v.Until.IsZero() {
		t.Fatal("until not parsed")
	}
	if got := v.Until.Format("2006-01-02"); got != "2026-12-31" {
		t.Errorf("until = %s, want 2026-12-31", got)
	}
	if v.Reason != "DEBT-412 needs the v2 migration" {
		t.Errorf("reason = %q — the until= should be stripped out of it", v.Reason)
	}
}

// Removing the date is itself the decision to make an exception permanent.
func TestNoUntilMeansPermanent(t *testing.T) {
	v, _ := Parse("// quality:accepted this is fine forever")
	if !v.Until.IsZero() {
		t.Error("no until= should mean permanent")
	}
	if v.Expired(time.Now().AddDate(10, 0, 0)) {
		t.Error("a permanent exception expired")
	}
}

func TestExpiredIsInclusiveOfTheDay(t *testing.T) {
	v, _ := Parse("// quality:accepted until=2026-06-15 x")
	onTheDay := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC)
	if v.Expired(onTheDay) {
		t.Error("expired on the review-by date itself; it should be inclusive")
	}
	if !v.Expired(time.Date(2026, 6, 16, 1, 0, 0, 0, time.UTC)) {
		t.Error("did not expire the day after")
	}
}

func TestConfigMatchesRuleAndPath(t *testing.T) {
	c := Config{Verdicts: []Rule{
		{Rule: "any-in-exported-signature", Path: "**/generated/**",
			Verdict: schema.FalsePositive, Reason: "generator conventions"},
		{Rule: "panic-in-library", Verdict: schema.WontFix, Reason: "deliberate"},
	}}

	if v, ok := c.Match("any-in-exported-signature", "internal/generated/api.go"); !ok ||
		v.Verdict != schema.FalsePositive {
		t.Errorf("path glob did not match: %+v ok=%v", v, ok)
	}
	if _, ok := c.Match("any-in-exported-signature", "internal/handwritten/api.go"); ok {
		t.Error("matched a path the rule does not cover")
	}
	// No path means the rule applies everywhere.
	if v, ok := c.Match("panic-in-library", "anywhere/at/all.go"); !ok || v.Verdict != schema.WontFix {
		t.Errorf("rule-only entry did not match everywhere: %+v ok=%v", v, ok)
	}
	if _, ok := c.Match("some-other-rule", "x.go"); ok {
		t.Error("matched an unrelated rule")
	}
}

// An ordered list is something a person can reason about by reading top to
// bottom; specificity scoring is clever and unpredictable.
func TestFirstMatchWins(t *testing.T) {
	c := Config{Verdicts: []Rule{
		{Rule: "r", Path: "a/**", Verdict: schema.FalsePositive, Reason: "first"},
		{Rule: "r", Verdict: schema.Accepted, Reason: "second"},
	}}
	if v, _ := c.Match("r", "a/b.go"); v.Reason != "first" {
		t.Errorf("reason = %q, want the first matching rule", v.Reason)
	}
	if v, _ := c.Match("r", "z/b.go"); v.Reason != "second" {
		t.Errorf("reason = %q, want the fallback rule", v.Reason)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"**/generated/**", "internal/generated/api.go", true},
		{"**/generated/**", "generated/api.go", true},
		{"**/generated/**", "internal/real/api.go", false},
		{"internal/**", "internal/a/b.go", true},
		{"internal/**", "cmd/a.go", false},
		{"*.go", "a.go", true},
		{"*.go", "a/b.go", false},
		{"**_test.go", "internal/a_test.go", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// An unexplained exception is indistinguishable from an oversight.
func TestValidateDemandsAReason(t *testing.T) {
	c := Config{Verdicts: []Rule{{Rule: "r", Verdict: schema.Accepted}}}
	errs := c.Validate()
	if len(errs) == 0 {
		t.Fatal("accepted a rule with no reason")
	}
}

func TestValidateCatchesBadInput(t *testing.T) {
	c := Config{Verdicts: []Rule{
		{Rule: "r", Verdict: "maybe", Reason: "x"},
		{Verdict: schema.Accepted, Reason: "x"},
		{Rule: "r", Verdict: schema.Accepted, Reason: "x", Until: "next tuesday"},
	}}
	if got := len(c.Validate()); got != 3 {
		t.Errorf("got %d errors, want 3 (bad verdict, no selector, bad date)", got)
	}
}

// Inferring "gmail" as an organisation would corrupt the one criterion that
// stops a rule being promoted on a single team's house style.
func TestOrgFromEmailDeclinesToGuess(t *testing.T) {
	for _, e := range []string{
		"sven@gmail.com", "x@outlook.com", "y@proton.me", "z@icloud.com",
		"8349202+sherzing@users.noreply.github.com",
		"not-an-email", "", "@nodomain", "trailing@",
	} {
		if got := OrgFromEmail(e); got != "" {
			t.Errorf("OrgFromEmail(%q) = %q, want empty — a wrong org is worse than none", e, got)
		}
	}
}

func TestOrgFromEmailFindsRealOrgs(t *testing.T) {
	cases := map[string]string{
		"sven@acme.com":           "acme",
		"a.b@engineering.acme.io": "acme",
		"x@acme.co.uk":            "acme",
		"y@acme.com.au":           "acme",
		"Z@ACME.COM":              "acme",
	}
	for email, want := range cases {
		if got := OrgFromEmail(email); got != want {
			t.Errorf("OrgFromEmail(%q) = %q, want %q", email, got, want)
		}
	}
}

// Explicit always wins: someone typing --org knows more than a domain heuristic.
func TestResolveOrgPrecedence(t *testing.T) {
	cases := []struct{ flag, cfg, email, want string }{
		{"flagorg", "cfgorg", "x@acme.com", "flagorg"},
		{"", "cfgorg", "x@acme.com", "cfgorg"},
		{"", "", "x@acme.com", "acme"},
		{"", "", "x@gmail.com", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := ResolveOrg(c.flag, c.cfg, c.email); got != c.want {
			t.Errorf("ResolveOrg(%q,%q,%q) = %q, want %q", c.flag, c.cfg, c.email, got, c.want)
		}
	}
}

func TestApplyStampsEveryField(t *testing.T) {
	var f model.Finding
	Apply(&f, V{Verdict: schema.WontFix, Reason: "guarded", Source: "config",
		Until: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	if f.Verdict != "wont-fix" || f.VerdictWhy != "guarded" || f.VerdictFrom != "config" || f.VerdictUntil != "2027-01-02" {
		t.Errorf("stamped %+v", f)
	}
	var g model.Finding
	Apply(&g, V{Verdict: schema.Accepted, Reason: "debt"})
	if g.VerdictUntil != "" {
		t.Errorf("a permanent verdict got an expiry: %q", g.VerdictUntil)
	}
}
