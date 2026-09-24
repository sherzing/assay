package schema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ts(t *testing.T) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, "2026-09-20T11:22:33Z")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func encode(t *testing.T, recs ...any) string {
	t.Helper()
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	for _, r := range recs {
		if err := e.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func decodeAll(t *testing.T, s string) ([]Record, map[int]error) {
	t.Helper()
	var got []Record
	errs := map[int]error{}
	err := Decode(strings.NewReader(s), func(r Record) error {
		got = append(got, r)
		return nil
	}, func(line int, err error) { errs[line] = err })
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got, errs
}

// THE LOAD-BEARING TEST.
//
// The package doc says the contract is the FORMAT, not this package — a tool in
// any language should compose with ours over a pipe. That promise is only kept
// if the JSON field names never move. A Go field rename is invisible to Go
// consumers (they recompile) and catastrophic for everyone else (they get a
// zero value with no error). Pinning the exact wire bytes is what makes such a
// change impossible to land by accident.
//
// If this test fails, you have changed the public contract. Bump Version and
// say so in the changelog — do not just update the golden string.
func TestWireFormatIsFrozen(t *testing.T) {
	golden := map[string]struct {
		rec  any
		want string
	}{
		"finding": {
			&Finding{
				Repo: "svc", Commit: "abc123", TS: ts(t), Tool: "ratchet",
				Rule: "no-panic", Severity: SevError, File: "a/b.go", Line: 12, Col: 3,
				Symbol: "Handle", Message: "boom", Suggest: "return an error",
				Fingerprint: "ff00", Verdict: Accepted, VerdictWhy: "legacy",
				VerdictUntil: "2027-01-01", VerdictFrom: "comment",
			},
			`{"v":1,"kind":"finding","repo":"svc","commit":"abc123","ts":"2026-09-20T11:22:33Z","tool":"ratchet","rule":"no-panic","severity":"error","file":"a/b.go","line":12,"col":3,"symbol":"Handle","message":"boom","suggest":"return an error","fingerprint":"ff00","verdict":"accepted","verdictReason":"legacy","verdictUntil":"2027-01-01","verdictSource":"comment"}`,
		},
		"measure": {
			&Measure{Repo: "svc", Commit: "abc123", TS: ts(t), Scope: ScopeFile,
				Path: "a/b.go", Metric: "cyclomatic", Value: 12.5},
			`{"v":1,"kind":"measure","repo":"svc","commit":"abc123","ts":"2026-09-20T11:22:33Z","scope":"file","path":"a/b.go","metric":"cyclomatic","value":12.5}`,
		},
		"verdict": {
			&Verdict{Fingerprint: "ff00", Rule: "no-panic", Repo: "svc", Org: "acme",
				Verdict: FalsePositive, Reason: "idiomatic", By: "someone", TS: ts(t)},
			`{"v":1,"kind":"verdict","fingerprint":"ff00","rule":"no-panic","repo":"svc","org":"acme","verdict":"false-positive","reason":"idiomatic","by":"someone","ts":"2026-09-20T11:22:33Z"}`,
		},
		"ticket": {
			&Ticket{Provider: "linear", ID: "ENG-1", URL: "https://example.invalid/ENG-1",
				Repo: "svc", GroupBy: "rule", Group: "no-panic", Title: "clean up",
				Fingerprints: []string{"ff00", "ff01"}, Resolved: []string{"ff00"},
				State: "open", TS: ts(t)},
			`{"v":1,"kind":"ticket","provider":"linear","id":"ENG-1","url":"https://example.invalid/ENG-1","repo":"svc","groupBy":"rule","group":"no-panic","title":"clean up","fingerprints":["ff00","ff01"],"resolved":["ff00"],"state":"open","ts":"2026-09-20T11:22:33Z"}`,
		},
	}
	for name, c := range golden {
		t.Run(name, func(t *testing.T) {
			if got := strings.TrimSpace(encode(t, c.rec)); got != c.want {
				t.Errorf("wire format changed — this breaks every non-Go consumer\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func TestRoundTripPreservesEveryField(t *testing.T) {
	in := []any{
		&Finding{Repo: "svc", TS: ts(t), Rule: "r", Severity: SevWarn, File: "f.go",
			Line: 1, Message: "m", Fingerprint: "fp"},
		&Measure{Repo: "svc", TS: ts(t), Scope: ScopeProject, Metric: "loc", Value: 42},
		&Verdict{Fingerprint: "fp", Verdict: WontFix, TS: ts(t)},
		&Ticket{Provider: "linear", ID: "E-1", GroupBy: "rule", Group: "r",
			Title: "t", Fingerprints: []string{"fp"}, State: "open", TS: ts(t)},
	}
	got, errs := decodeAll(t, encode(t, in...))
	if len(errs) != 0 {
		t.Fatalf("unexpected decode errors: %v", errs)
	}
	if len(got) != 4 {
		t.Fatalf("got %d records, want 4", len(got))
	}
	for i, want := range in {
		var have any
		switch {
		case got[i].Finding != nil:
			have = got[i].Finding
		case got[i].Measure != nil:
			have = got[i].Measure
		case got[i].Verdict != nil:
			have = got[i].Verdict
		case got[i].Ticket != nil:
			have = got[i].Ticket
		default:
			t.Fatalf("record %d has no non-nil field", i)
		}
		w, _ := json.Marshal(want)
		h, _ := json.Marshal(have)
		if string(w) != string(h) {
			t.Errorf("record %d round-trip lost data\n got: %s\nwant: %s", i, h, w)
		}
	}
}

// Exactly one field must be non-nil, or a consumer switching on the fields can
// silently process a record twice.
func TestRecordIsADiscriminatedUnion(t *testing.T) {
	got, _ := decodeAll(t, encode(t,
		&Finding{Rule: "r", File: "f", Fingerprint: "fp", TS: ts(t)},
		&Measure{Repo: "r", Metric: "m", TS: ts(t)},
	))
	for i, r := range got {
		n := 0
		for _, nonNil := range []bool{r.Finding != nil, r.Measure != nil, r.Verdict != nil, r.Ticket != nil} {
			if nonNil {
				n++
			}
		}
		if n != 1 {
			t.Errorf("record %d has %d non-nil fields, want exactly 1", i, n)
		}
	}
}

// Write sets Kind and V itself so no caller can emit a line nothing downstream
// can route. A caller setting them wrongly must be overridden, not trusted.
func TestWriteOverridesKindAndVersion(t *testing.T) {
	f := &Finding{V: 99, Kind: "nonsense", Rule: "r", File: "f", Fingerprint: "fp", TS: ts(t)}
	out := encode(t, f)
	if !strings.Contains(out, `"kind":"finding"`) || !strings.Contains(out, `"v":1`) {
		t.Errorf("Write did not override a wrong kind/version: %s", out)
	}
	if f.Kind != KindFinding || f.V != Version {
		t.Errorf("Write left the struct with kind=%q v=%d", f.Kind, f.V)
	}
}

func TestWriteRejectsForeignTypes(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	for _, v := range []any{
		Finding{}, // value, not pointer: cannot have Kind stamped, must be refused
		map[string]any{"kind": "finding"},
		"a string",
		nil,
	} {
		if err := e.Write(v); err == nil {
			t.Errorf("Write(%T) succeeded; anything not a known record pointer must be refused", v)
		}
	}
}

// FORWARD COMPATIBILITY. An older reader must survive a stream written by a
// newer tool: one unknown kind cannot be allowed to kill a long pipe, and —
// the part that actually matters — must not swallow the valid records after it.
func TestUnknownKindIsSkippedWithoutLosingNeighbours(t *testing.T) {
	stream := encode(t, &Measure{Repo: "a", Metric: "m", Value: 1, TS: ts(t)}) +
		`{"v":2,"kind":"prophecy","foretells":"the future"}` + "\n" +
		encode(t, &Measure{Repo: "b", Metric: "m", Value: 2, TS: ts(t)})

	got, errs := decodeAll(t, stream)
	if len(got) != 2 {
		t.Fatalf("got %d records, want the 2 known ones either side of the unknown kind", len(got))
	}
	if got[0].Measure.Repo != "a" || got[1].Measure.Repo != "b" {
		t.Errorf("wrong records survived: %v", got)
	}
	if len(errs) != 0 {
		t.Errorf("an unknown kind is not an error, it is a newer tool: %v", errs)
	}
}

// Same requirement for genuine corruption, except it IS reported. A truncated
// line in the middle of a partition must not cost us the rest of the file.
func TestMalformedLineIsReportedAndSkipped(t *testing.T) {
	stream := encode(t, &Measure{Repo: "a", Metric: "m", Value: 1, TS: ts(t)}) +
		`{"kind":"measure","value":` + "\n" + // truncated mid-write
		`not json at all` + "\n" +
		encode(t, &Measure{Repo: "b", Metric: "m", Value: 2, TS: ts(t)})

	got, errs := decodeAll(t, stream)
	if len(got) != 2 {
		t.Fatalf("got %d records, want the 2 valid ones; corruption must not cascade", len(got))
	}
	if got[1].Measure.Repo != "b" {
		t.Error("the record after the corruption was lost — the worst possible failure here")
	}
	for _, line := range []int{2, 3} {
		if _, ok := errs[line]; !ok {
			t.Errorf("line %d was malformed but not reported; silent data loss", line)
		}
	}
}

func TestBlankLinesAndCommentsAreIgnored(t *testing.T) {
	stream := "\n  \n# a comment\n" +
		encode(t, &Measure{Repo: "a", Metric: "m", TS: ts(t)}) +
		"\n#another\n"
	got, errs := decodeAll(t, stream)
	if len(got) != 1 || len(errs) != 0 {
		t.Errorf("got %d records and %v errors, want 1 record and none", len(got), errs)
	}
}

// A nil onErr must not panic — plenty of callers do not care about the count.
func TestNilErrorHandlerIsSafe(t *testing.T) {
	err := Decode(strings.NewReader("garbage\n"), func(Record) error { return nil }, nil)
	if err != nil {
		t.Errorf("Decode with nil onErr: %v", err)
	}
}

// The callback's error aborts the stream. This is how a consumer stops early
// (head -n, a failed write downstream) without reading a million lines.
func TestCallbackErrorStopsDecoding(t *testing.T) {
	stream := strings.Repeat(encode(t, &Measure{Repo: "a", Metric: "m", TS: ts(t)}), 5)
	seen := 0
	err := Decode(strings.NewReader(stream), func(Record) error {
		seen++
		return errStop
	}, nil)
	if err != errStop {
		t.Errorf("err = %v, want the callback's own error propagated", err)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times after returning an error, want 1", seen)
	}
}

var errStop = &stopErr{}

type stopErr struct{}

func (*stopErr) Error() string { return "stop" }

// Findings carry rule messages and code snippets. The scanner buffer is raised
// to 8 MB for exactly this reason, and the default 64 KB would silently
// truncate the stream — bufio.Scanner reports ErrTooLong and stops, which would
// look like a short file rather than an error.
func TestLongLinesSurvive(t *testing.T) {
	long := strings.Repeat("x", 512*1024)
	got, errs := decodeAll(t, encode(t, &Finding{
		Rule: "r", File: "f", Fingerprint: "fp", TS: ts(t), Message: long,
	}))
	if len(errs) != 0 {
		t.Fatalf("decode errors on a large record: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 — the scanner buffer is too small", len(got))
	}
	if got[0].Finding.Message != long {
		t.Errorf("message truncated: got %d bytes, want %d", len(got[0].Finding.Message), len(long))
	}
}

// JSONL's one structural rule: a record may never contain a raw newline.
// encoding/json escapes them, but a hand-rolled writer would not, and the
// result splits one record into two unparseable halves.
func TestEmbeddedNewlinesDoNotSplitRecords(t *testing.T) {
	nasty := "line one\nline two\r\nwith \"quotes\" and a tab\t and ünïcode 🔍"
	out := encode(t, &Finding{Rule: "r", File: "f", Fingerprint: "fp", TS: ts(t), Message: nasty})
	if n := strings.Count(strings.TrimSuffix(out, "\n"), "\n"); n != 0 {
		t.Fatalf("one record spans %d extra lines; the stream is corrupt", n)
	}
	got, errs := decodeAll(t, out)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("got %d records, %v errors", len(got), errs)
	}
	if got[0].Finding.Message != nasty {
		t.Errorf("message mangled:\n got: %q\nwant: %q", got[0].Finding.Message, nasty)
	}
}

// Unjudged is deliberately distinct from accepted: an unexamined finding is not
// evidence either way. If the empty verdict were serialised, a consumer could
// not tell "nobody looked" from "somebody said yes".
func TestUnjudgedFindingOmitsTheVerdictEntirely(t *testing.T) {
	out := encode(t, &Finding{Rule: "r", File: "f", Fingerprint: "fp", TS: ts(t)})
	if strings.Contains(out, "verdict") {
		t.Errorf("an unjudged finding serialised a verdict field: %s", out)
	}
}

// Every judgement constant must survive the wire, or a triage session's results
// silently degrade into a different bucket.
func TestEveryJudgementRoundTrips(t *testing.T) {
	for _, j := range []Judgement{Accepted, FalsePositive, WontFix} {
		got, _ := decodeAll(t, encode(t, &Verdict{Fingerprint: "fp", Verdict: j, TS: ts(t)}))
		if len(got) != 1 || got[0].Verdict.Verdict != j {
			t.Errorf("judgement %q did not round-trip: %+v", j, got)
		}
	}
}

// Timestamps must be UTC RFC3339 on the wire. A local-time offset makes two
// contributors' evidence sort wrongly against each other.
func TestTimestampsSerialiseAsRFC3339UTC(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	local := ts(t).In(loc)
	out := encode(t, &Measure{Repo: "r", Metric: "m", TS: local})

	got, _ := decodeAll(t, out)
	if !got[0].Measure.TS.Equal(ts(t)) {
		t.Errorf("timestamp shifted across the wire: got %v, want %v", got[0].Measure.TS, ts(t))
	}
}

// The scopes are part of the contract: a consumer in another language switches on them, so a
// new one is a recorded decision, not a comment.
func TestScopesArePinned(t *testing.T) {
	want := []Scope{"project", "module", "file", "function", "class"}
	got := []Scope{ScopeProject, ScopeModule, ScopeFile, ScopeFunction, ScopeClass}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scope %d = %q, want %q", i, got[i], want[i])
		}
	}
}
