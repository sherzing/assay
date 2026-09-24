// Package sarif ingests SARIF 2.1.0 so the ratchet works on any language.
//
// The valuable part of this tool was never its detectors — it is the mechanism:
// a fingerprinted baseline that tolerates what exists and fails only on what is
// new, and that cannot be silently reset. That mechanism is language-agnostic.
// It only needs findings from somewhere.
//
// So rather than write a Dart analyser and a C# analyser and maintain both
// forever, we consume what each language's native tooling already emits:
// golangci-lint --out-format sarif, Roslyn analyzers, semgrep --sarif, CodeQL,
// dart analyze via a converter. Better fidelity than anything we would write,
// and no new parsers to keep alive.
package sarif

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sherzing/assay/internal/model"
)

// Minimal SARIF 2.1.0 subset. Deliberately partial: we decode only the fields
// that survive into a Finding, so an unfamiliar producer cannot break the parse.
type doc struct {
	Version string `json:"version"`
	Runs    []run  `json:"runs"`
}

type run struct {
	Tool struct {
		Driver struct {
			Name           string `json:"name"`
			InformationURI string `json:"informationUri"`
			Rules          []struct {
				ID               string `json:"id"`
				Name             string `json:"name"`
				ShortDescription struct {
					Text string `json:"text"`
				} `json:"shortDescription"`
				HelpURI          string `json:"helpUri"`
				DefaultConfigure struct {
					Level string `json:"level"`
				} `json:"defaultConfiguration"`
			} `json:"rules"`
		} `json:"driver"`
	} `json:"tool"`
	Results         []result `json:"results"`
	OriginalURIBase map[string]struct {
		URI string `json:"uri"`
	} `json:"originalUriBaseIds"`
}

type result struct {
	RuleID  string `json:"ruleId"`
	Level   string `json:"level"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI       string `json:"uri"`
				URIBaseID string `json:"uriBaseId"`
			} `json:"artifactLocation"`
			Region struct {
				StartLine   int `json:"startLine"`
				StartColumn int `json:"startColumn"`
				Snippet     struct {
					Text string `json:"text"`
				} `json:"snippet"`
			} `json:"region"`
		} `json:"physicalLocation"`
		LogicalLocations []struct {
			Name               string `json:"name"`
			FullyQualifiedName string `json:"fullyQualifiedName"`
			Kind               string `json:"kind"`
		} `json:"logicalLocations"`
	} `json:"locations"`
	// Suppressions is how SARIF says "the author excused this". Honouring it is
	// the same principle as honouring //nolint: a tool that cannot be told no
	// gets switched off entirely.
	Suppressions []struct {
		Kind          string `json:"kind"`
		Justification string `json:"justification"`
	} `json:"suppressions"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Fingerprints        map[string]string `json:"fingerprints"`
}

// Options controls import.
type Options struct {
	// Root is the repository root. Absolute SARIF paths are made relative to it
	// so imported findings key the same way as native ones.
	Root string
	// ToolPrefix overrides the tool name used to namespace rule IDs.
	ToolPrefix string
	// IncludeSuppressed imports results the producer marked as suppressed.
	IncludeSuppressed bool
}

// Report is what one SARIF document yields: its findings, and the tools that ran, so a run
// with no results can still be counted as a zero for its producer.
type Report struct {
	Findings []model.Finding
	Tools    []string // driver names, ToolPrefix applied, in run order without repeats
}

// ImportReport reads SARIF and returns the findings and the tools that ran.
func ImportReport(r io.Reader, opt Options) (*Report, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read sarif: %w", err)
	}
	findings, err := Import(bytes.NewReader(raw), opt)
	if err != nil {
		return nil, err
	}
	return &Report{Findings: findings, Tools: tools(raw, opt)}, nil
}

// toolName is the producer a run's rules are namespaced by.
func toolName(driver string, opt Options) string {
	if opt.ToolPrefix != "" {
		return opt.ToolPrefix
	}
	if driver == "" {
		return "sarif"
	}
	return driver
}

// tools lists the producers a document names, so a clean run still has a series to be zero in.
func tools(raw []byte, opt Options) []string {
	var d struct {
		Runs []struct {
			Tool struct {
				Driver struct {
					Name string `json:"name"`
				} `json:"driver"`
			} `json:"tool"`
		} `json:"runs"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, rn := range d.Runs {
		if t := toolName(rn.Tool.Driver.Name, opt); !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Import reads SARIF and returns findings.
func Import(r io.Reader, opt Options) ([]model.Finding, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read sarif: %w", err)
	}

	// THE VERSION IS CHECKED BEFORE THE FULL DECODE, not after. SARIF 1.0
	// carries `message` as a string where 2.1 has an object, so decoding a 1.0
	// document into these types fails with
	//
	//   json: cannot unmarshal string into Go struct field ... of type struct { Text string }
	//
	// which is a true statement about Go and no help at all to someone holding
	// the output of a .NET build. A version probe first means the diagnosis
	// comes out instead of the symptom.
	var probe struct {
		Version string          `json:"version"`
		Runs    json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse sarif: %w", err)
	}
	if strings.TrimSpace(probe.Version) == "" && len(probe.Runs) == 0 {
		return nil, fmt.Errorf("not a SARIF document: it has neither a version nor runs")
	}
	d := doc{Version: probe.Version}

	// REFUSE ANYTHING BUT 2.1.0. SARIF 1.0 nests results under a different
	// shape entirely, so this decoder finds nothing in one and reports
	// "imported 0 findings" — a clean bill of health for a file it could not
	// read.
	//
	// Not hypothetical: Roslyn's `-p:ErrorLog=out.sarif` emits 1.0 by DEFAULT.
	// The obvious way to get diagnostics out of a .NET build produces exactly
	// the file this used to swallow. You have to ask for
	// `-p:ErrorLog=out.sarif,version=2.1` to get 2.1.0.
	if v := strings.TrimSpace(d.Version); v != "" && !strings.HasPrefix(v, "2.") {
		return nil, fmt.Errorf("sarif version %q is not supported — this reads 2.1.0.\n"+
			"  If this came from a .NET build, Roslyn defaults to SARIF 1.0; ask for 2.1 with\n"+
			"    dotnet build -p:ErrorLog=out.sarif%%2cversion=2.1   (%%2c, not a comma)", v)
	}

	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse sarif: %w", err)
	}
	if len(d.Runs) == 0 {
		return nil, nil
	}

	var out []model.Finding
	for _, rn := range d.Runs {
		tool := toolName(rn.Tool.Driver.Name, opt)

		// Rule metadata gives us a description and a default severity for
		// results that omit `level`.
		type meta struct{ desc, level, help string }
		rules := map[string]meta{}
		for _, ru := range rn.Tool.Driver.Rules {
			rules[ru.ID] = meta{ru.ShortDescription.Text, ru.DefaultConfigure.Level, ru.HelpURI}
		}

		for _, res := range rn.Results {
			if len(res.Suppressions) > 0 && !opt.IncludeSuppressed {
				continue
			}
			f := convert(res, tool, rules[res.RuleID].desc, rules[res.RuleID].level, opt)
			if f.File == "" {
				// A result with no location cannot be fingerprinted stably and
				// cannot be acted on. Dropping it is better than carrying a
				// finding nobody can find.
				continue
			}
			out = append(out, f)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Rule < b.Rule
	})
	return out, nil
}

func convert(res result, tool, desc, defLevel string, opt Options) model.Finding {
	var file, snippet, fn string
	var line, col int
	if len(res.Locations) > 0 {
		loc := res.Locations[0]
		file = normalisePath(loc.PhysicalLocation.ArtifactLocation.URI, opt.Root)
		line = loc.PhysicalLocation.Region.StartLine
		col = loc.PhysicalLocation.Region.StartColumn
		snippet = loc.PhysicalLocation.Region.Snippet.Text
		// A logical location (method/class name) is the best available stand-in
		// for ratchet's "enclosing function", and it is what makes an imported
		// fingerprint survive the file being reordered.
		if len(loc.LogicalLocations) > 0 {
			fn = loc.LogicalLocations[0].FullyQualifiedName
			if fn == "" {
				fn = loc.LogicalLocations[0].Name
			}
		}
	}

	// Namespace the rule by tool. Without this, two tools that both emit a rule
	// called "CS0168" or "unused" would collide in one baseline and silently
	// tolerate each other's violations.
	rule := tool + ":" + res.RuleID
	if res.RuleID == "" {
		rule = tool + ":unknown"
	}

	msg := res.Message.Text
	if msg == "" {
		msg = desc
	}

	level := res.Level
	if level == "" {
		level = defLevel
	}

	// Fingerprint preference order:
	//  1. the producer's own stable fingerprint, if it supplies one
	//  2. our own hash, which deliberately excludes the line number
	// Either way a reformat must not read as a wave of new violations.
	fp := pickFingerprint(res)
	if fp == "" {
		basis := snippet
		if basis == "" {
			// No snippet: fall back to the message, which for most linters is
			// stable for a given defect and varies between different defects.
			basis = msg
		}
		fp = model.Fingerprint(file, rule, fn, basis)
	}

	return model.Finding{
		Rule:        rule,
		Severity:    severity(level),
		File:        file,
		Line:        line,
		Col:         col,
		Func:        fn,
		Message:     msg,
		Fingerprint: fp,
	}
}

// pickFingerprint prefers a producer-supplied stable identifier. CodeQL and
// several others emit partialFingerprints designed for exactly this purpose.
func pickFingerprint(res result) string {
	for _, key := range []string{"primaryLocationLineHash", "ratchet/v1"} {
		if v, ok := res.PartialFingerprints[key]; ok && v != "" {
			return short(v)
		}
	}
	// Any single deterministic value is better than none, but map iteration is
	// random, so take the lowest key for stability across runs.
	if len(res.Fingerprints) > 0 {
		keys := make([]string, 0, len(res.Fingerprints))
		for k := range res.Fingerprints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return short(res.Fingerprints[keys[0]])
	}
	return ""
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// severity maps SARIF levels. SARIF's default when absent is "warning".
func severity(level string) model.Severity {
	switch strings.ToLower(level) {
	case "error":
		return model.Error
	case "note", "none":
		return model.Info
	default:
		return model.Warn
	}
}

// normalisePath turns a SARIF artifact URI into a repo-relative path, so an
// imported finding keys identically to a native one. SARIF producers variously
// emit file:// URIs, absolute paths, and already-relative paths.
func normalisePath(uri, root string) string {
	p := strings.TrimPrefix(uri, "file://")
	if p == "" {
		return ""
	}
	if root != "" && filepath.IsAbs(p) {
		if abs, err := filepath.Abs(root); err == nil {
			if rel, err := filepath.Rel(abs, p); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(strings.TrimPrefix(p, "./"))
}

// ImportFile is a convenience wrapper.
func ImportFile(path string, opt Options) ([]model.Finding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Import(f, opt)
}
