package arch

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, body string) *Decl {
	t.Helper()
	d, err := Parse(body, 1)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func TestParseLayersAndForbids(t *testing.T) {
	d := mustParse(t, `
layer domain   internal/domain
layer infra    internal/impl internal/store   # two paths on one layer
forbid domain -> infra
`)
	if got := d.Layers["domain"]; len(got) != 1 || got[0] != "internal/domain" {
		t.Errorf("domain layer = %v", got)
	}
	if got := d.Layers["infra"]; len(got) != 2 {
		t.Errorf("infra layer = %v, want two paths", got)
	}
	if len(d.Forbids) != 1 || d.Forbids[0].From != "domain" || d.Forbids[0].To != "infra" {
		t.Errorf("forbids = %+v", d.Forbids)
	}
	if d.Order[0] != "domain" || d.Order[1] != "infra" {
		t.Errorf("declaration order not preserved: %v", d.Order)
	}
}

// THE LOAD-BEARING TEST for parsing.
//
// A document with no arch block must be an ERROR. Returning "no violations"
// would make CI green for a repository that enforces nothing, and everyone
// would believe the architecture was checked. Silence is the worst answer here.
func TestDocumentWithNoBlockIsAnError(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":            "",
		"prose only":       "# Architecture\n\nWe are layered, honest.\n",
		"wrong fence":      "```yaml\nlayer domain x\n```\n",
		"unclosed fence":   "```arch\nlayer domain internal/domain\n",
		"block but empty":  "```arch\n```\n",
		"only whitespace":  "```arch\n\n   \n```\n",
		"layers no forbid": "```arch\nlayer domain internal/domain\n```\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDoc(doc); err == nil {
				t.Error("parsed without error; a document that enforces nothing must say so")
			}
		})
	}
}

// A misspelled keyword must not be skipped. Silently ignoring `forbidd` drops a
// constraint and nobody finds out until something breaks.
func TestUnknownStatementIsAnError(t *testing.T) {
	_, err := Parse("layer a x\nlayer b y\nforbidd a -> b\n", 1)
	if err == nil {
		t.Fatal("a misspelled keyword was silently ignored — the rule would just vanish")
	}
	if !strings.Contains(err.Error(), "forbidd") {
		t.Errorf("error does not name the offending token: %v", err)
	}
}

func TestParseErrorsNameTheLine(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"undeclared from":  {"layer b y\nforbid a -> b\n", "line 2"},
		"undeclared to":    {"layer a x\nforbid a -> b\n", "line 2"},
		"self forbid":      {"layer a x\nforbid a -> a\n", "line 2"},
		"layer no path":    {"layer a\nforbid a -> a\n", "line 1"},
		"malformed forbid": {"layer a x\nlayer b y\nforbid a b\n", "line 3"},
		"duplicate layer":  {"layer a x\nlayer a z\nforbid a -> a\n", "line 2"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(c.body, 1)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not report %s", err, c.want)
			}
		})
	}
}

// Line numbers must point into the markdown file, not into the extracted
// fragment, or the reader goes hunting.
func TestLineNumbersAreRelativeToTheDocument(t *testing.T) {
	doc := "# Architecture\n\nSome prose.\n\nMore prose.\n\n```arch\nlayer a x\nforbid a -> nope\n```\n"
	_, err := ParseDoc(doc)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 9") {
		t.Errorf("error should point at line 9 of the document, got: %v", err)
	}
}

func TestCommentsAndBlankLinesIgnored(t *testing.T) {
	d := mustParse(t, "# top comment\n\nlayer a x   # trailing\n\nlayer b y\nforbid a -> b # why\n")
	if len(d.Layers) != 2 || len(d.Forbids) != 1 {
		t.Errorf("got %d layers %d forbids", len(d.Layers), len(d.Forbids))
	}
}

func TestTrailingSlashesNormalised(t *testing.T) {
	d := mustParse(t, "layer a /internal/domain/\nlayer b y\nforbid a -> b\n")
	if d.Layers["a"][0] != "internal/domain" {
		t.Errorf("path not normalised: %q", d.Layers["a"][0])
	}
}

// Longest prefix wins, so a nested layer beats its parent. Without this the
// answer would depend on declaration order, which is a surprising thing for an
// architecture to hinge on.
func TestLayerOfPrefersTheLongestMatch(t *testing.T) {
	d := mustParse(t, `
layer domain    internal/domain
layer billing   internal/domain/billing
layer infra     internal/impl
forbid domain -> infra
`)
	cases := map[string]string{
		"internal/domain":             "domain",
		"internal/domain/order":       "domain",
		"internal/domain/billing":     "billing",
		"internal/domain/billing/tax": "billing",
		"internal/impl":               "infra",
	}
	for pkg, want := range cases {
		got, ok := d.LayerOf(pkg)
		if !ok || got != want {
			t.Errorf("LayerOf(%q) = %q (%v), want %q", pkg, got, ok, want)
		}
	}
	if _, ok := d.LayerOf("cmd/server"); ok {
		t.Error("an unlayered package must not match")
	}
	// A prefix must not match a sibling with a shared name stem.
	if _, ok := d.LayerOf("internal/domainhelper"); ok {
		t.Error("internal/domainhelper matched the internal/domain layer — prefix matching is not path-aware")
	}
}

// The first block wins and a later fenced example must not be swallowed.
func TestOnlyTheFirstBlockIsRead(t *testing.T) {
	doc := "```arch\nlayer a x\nlayer b y\nforbid a -> b\n```\n\n## Example\n\n```sh\nplumb check .\n```\n"
	d, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Forbids) != 1 {
		t.Errorf("got %d forbids, want 1 — a later fenced block leaked in", len(d.Forbids))
	}
}

// group is the keyword; layer is the synonym the first release shipped with.
// Both must parse to the same declaration, so no document has to change.
func TestGroupAndLayerAreSynonyms(t *testing.T) {
	g, err := Parse("group a x\ngroup b y\nforbid a -> b\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse("layer a x\nlayer b y\nforbid a -> b\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := Diff(g, l); c != NoChange {
		t.Errorf("group and layer parsed differently: %s", c)
	}
}
