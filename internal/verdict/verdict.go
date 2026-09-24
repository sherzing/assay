// Package verdict resolves human judgements about findings.
//
// A judgement can come from two places, and they are good at different things:
//
//   - AN IN-CODE COMMENT, for a specific instance. The reason sits next to the
//     thing it excuses, and a reviewer sees both in the same diff. This is the
//     strongest review path there is — far better than a hash in a JSON file.
//   - A PROJECT CONFIG, for a pattern. "This rule does not apply under
//     internal/impl/postgres" is one line, where annotating it would be 97
//     comments. Blanket judgements belong in one reviewable place.
//
// THE COMMENT SYNTAX IS DELIBERATELY NOT TOOL-BRANDED. A `// assay:` prefix
// would say "this is for one vendor's tool" and make every other analyser ignore
// it. `// quality:` describes the domain instead, so another tool can read the
// same annotation and honour the same decision. We are trying to establish a
// convention, not a moat.
package verdict

import (
	"fmt"
	"github.com/sherzing/assay/internal/model"
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// Marker is the neutral comment prefix.
const Marker = "quality:"

// V is a resolved judgement with its provenance.
type V struct {
	Verdict schema.Judgement
	Reason  string
	Until   time.Time // zero means permanent
	Source  string    // "comment" or "config"
	Line    int       // for comment-sourced, so errors can point at it
}

// Expired reports whether a review-by date has passed.
//
// An accepted finding is debt someone agreed to carry, and an agreement with no
// end date quietly becomes permanent. A review-by date forces the decision to be
// made again rather than drift — and removing the date is itself the decision to
// make it permanent, which is fine as long as it was deliberate.
func (v V) Expired(now time.Time) bool {
	return !v.Until.IsZero() && now.After(v.Until)
}

// quality:<verdict> [until=YYYY-MM-DD] <reason>
var re = regexp.MustCompile(`(?i)\bquality:(false-positive|accepted|wont-fix)\b\s*(.*)`)
var untilRe = regexp.MustCompile(`(?i)\buntil=(\d{4}-\d{2}-\d{2})\b`)

// Parse reads one comment line. ok is false when it carries no marker.
func Parse(text string) (V, bool) {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return V{}, false
	}
	v := V{Verdict: schema.Judgement(strings.ToLower(m[1])), Source: "comment"}
	rest := strings.TrimSpace(m[2])

	if u := untilRe.FindStringSubmatch(rest); u != nil {
		if t, err := time.Parse("2006-01-02", u[1]); err == nil {
			v.Until = t.Add(24*time.Hour - time.Nanosecond) // inclusive of that day
		}
		rest = strings.TrimSpace(untilRe.ReplaceAllString(rest, ""))
	}
	v.Reason = strings.TrimLeft(rest, " -—:")
	return v, true
}

// FromComments indexes annotations by the line they apply to.
//
// An annotation covers its own line and the line after, matching how //nolint
// behaves — people write it either at the end of the offending line or on the
// line above the declaration.
func FromComments(fset *token.FileSet, f *ast.File) map[int]V {
	out := map[int]V{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			v, ok := Parse(c.Text)
			if !ok {
				continue
			}
			line := fset.Position(c.Pos()).Line
			v.Line = line
			out[line] = v
			if _, taken := out[line+1]; !taken {
				out[line+1] = v
			}
		}
	}
	return out
}

// Rule is a pattern-matched judgement from project config.
type Rule struct {
	Rule    string           `yaml:"rule" json:"rule"`
	Path    string           `yaml:"path" json:"path"`
	Verdict schema.Judgement `yaml:"verdict" json:"verdict"`
	Reason  string           `yaml:"reason" json:"reason"`
	Until   string           `yaml:"until" json:"until"`
}

// Config is the project-level source.
type Config struct {
	// Org attributes this repo's evidence. Set it once per repo rather than
	// passing --org on every invocation.
	Org      string `yaml:"org" json:"org"`
	Verdicts []Rule `yaml:"verdicts" json:"verdicts"`
}

// ResolveOrg picks an organisation from the available sources, most reliable
// first: an explicit flag, then project config, then an inference from the
// committer's email domain.
//
// The inference is last and it declines to guess: a consumer mail provider
// yields nothing rather than a wrong answer, because no attribution is honest
// and a wrong one quietly inflates the cross-organisation count that gates rule
// promotion.
func ResolveOrg(flag, cfg, email string) string {
	if flag != "" {
		return flag
	}
	if cfg != "" {
		return cfg
	}
	return OrgFromEmail(email)
}

// Match returns the first rule that covers this finding.
//
// First match wins rather than most-specific, because an ordered list is
// something a person can reason about by reading top to bottom. Specificity
// scoring is clever and nobody can predict it.
func (c Config) Match(ruleID, file string) (V, bool) {
	for _, r := range c.Verdicts {
		if r.Rule != "" && r.Rule != ruleID && !globMatch(r.Rule, ruleID) {
			continue
		}
		if r.Path != "" && !globMatch(r.Path, file) {
			continue
		}
		v := V{Verdict: r.Verdict, Reason: r.Reason, Source: "config"}
		if r.Until != "" {
			if t, err := time.Parse("2006-01-02", r.Until); err == nil {
				v.Until = t.Add(24*time.Hour - time.Nanosecond)
			}
		}
		return v, true
	}
	return V{}, false
}

// globMatch translates a glob to a regexp rather than walking indexes.
//
// The hand-rolled version got `**/generated/**` wrong against `generated/a.go`,
// because `**/` has to match ZERO segments as well as many. Translation makes
// that case fall out of the grammar instead of needing a special case:
//
//	**/   zero or more path segments
//	**    anything, including separators
//	*     anything except a separator
//	?     one character except a separator
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	re, ok := globCache[pattern]
	if !ok {
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(pattern); i++ {
			switch {
			case strings.HasPrefix(pattern[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 2
			case strings.HasPrefix(pattern[i:], "**"):
				b.WriteString(".*")
				i++
			case pattern[i] == '*':
				b.WriteString("[^/]*")
			case pattern[i] == '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			}
		}
		b.WriteString("$")
		var err error
		re, err = regexp.Compile(b.String())
		if err != nil {
			return false
		}
		globCache[pattern] = re
	}
	return re.MatchString(s)
}

// globCache avoids recompiling the same handful of patterns for every finding.
// Not concurrency-safe, and does not need to be: config is read once per scan.
var globCache = map[string]*regexp.Regexp{}

// Validate reports problems a human should fix rather than have silently ignored.
func (c Config) Validate() []error {
	var errs []error
	for i, r := range c.Verdicts {
		switch r.Verdict {
		case schema.Accepted, schema.FalsePositive, schema.WontFix:
		default:
			errs = append(errs, fmt.Errorf("verdicts[%d]: unknown verdict %q (want accepted, false-positive or wont-fix)", i, r.Verdict))
		}
		if r.Rule == "" && r.Path == "" {
			errs = append(errs, fmt.Errorf("verdicts[%d]: needs at least one of rule or path, or it matches everything", i))
		}
		if r.Until != "" {
			if _, err := time.Parse("2006-01-02", r.Until); err != nil {
				errs = append(errs, fmt.Errorf("verdicts[%d]: bad until %q, want YYYY-MM-DD", i, r.Until))
			}
		}
		if r.Reason == "" {
			errs = append(errs, fmt.Errorf("verdicts[%d]: needs a reason — an unexplained exception is indistinguishable from an oversight", i))
		}
	}
	return errs
}

// Apply stamps a verdict on a finding, so the scan and the importers cannot drift apart.
func Apply(f *model.Finding, v V) {
	f.Verdict, f.VerdictWhy, f.VerdictFrom = string(v.Verdict), v.Reason, v.Source
	if !v.Until.IsZero() {
		f.VerdictUntil = v.Until.Format("2006-01-02")
	}
}
