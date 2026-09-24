package dcm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Sniff reports whether data is a DCM report, by the formatVersion at its root.
func Sniff(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte(`"formatVersion"`))
}

type fileResult struct {
	Path   string          `json:"path"`
	Issues json.RawMessage `json:"issues"`
}

type issue struct {
	ID              string          `json:"id"`
	Location        location        `json:"location"`
	DeclarationName string          `json:"declarationName"`
	DeclarationType string          `json:"declarationType"`
	Value           json.RawMessage `json:"value"`
}

type location struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
}

type fileIssues struct {
	path   string
	issues []issue
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// num reads a metric value; an unreadable one is skipped, not fatal.
func num(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	if s := str(raw); s != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// readFiles decodes one *Results section; a missing section is empty.
func readFiles(raw json.RawMessage) ([]fileIssues, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var frs []fileResult
	if err := json.Unmarshal(raw, &frs); err != nil {
		return nil, err
	}
	out := make([]fileIssues, 0, len(frs))
	for _, fr := range frs {
		issues, err := readIssues(fr.Issues)
		if err != nil {
			return nil, fmt.Errorf("%s: issues: %w", fr.Path, err)
		}
		out = append(out, fileIssues{path: filepath.ToSlash(fr.Path), issues: issues})
	}
	return out, nil
}

// readIssues accepts an array or a single object; the docs show both.
func readIssues(raw json.RawMessage) ([]issue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var many []issue
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one issue
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []issue{one}, nil
}

// countIssues counts a section's issues without reading them.
func countIssues(raw json.RawMessage) (int, error) {
	files, err := readFiles(raw)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		n += len(f.issues)
	}
	return n, nil
}
