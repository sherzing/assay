package analyze

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
)

// Rule identifiers. Stable strings — they end up in baselines checked into git,
// so renaming one silently retires every recorded violation under the old name.
const (
	RuleNakedAssert   = "naked-type-assertion"
	RuleErrSwallowed  = "error-swallowed"
	RuleAnyExported   = "any-in-exported-signature"
	RulePanicInLib    = "panic-in-library"
	RuleElseAfterJump = "else-after-return"
)

// Rules is the registry. Each is individually toggleable because some will be
// wrong for a given codebase, and a rule you cannot switch off is a rule that
// gets the whole tool switched off.
var Rules = map[string]struct {
	Severity model.Severity
	Doc      string
}{
	RuleNakedAssert: {model.Error,
		"Type assertion without the comma-ok form panics at runtime. The Go analogue of a cast added to satisfy a checker."},
	RuleErrSwallowed: {model.Error,
		"An error is discarded. The failure becomes invisible rather than handled."},
	RuleAnyExported: {model.Warn,
		"interface{}/any in an exported signature pushes type checking to runtime and to the caller."},
	RulePanicInLib: {model.Warn,
		"panic() in library code removes the caller's ability to decide. Return an error."},
	RuleElseAfterJump: {model.Info,
		"else after a terminating branch adds nesting for no reason."},
}

// smellPass collects findings for one file.
type smellPass struct {
	fset     *token.FileSet
	file     *ast.File
	relPath  string
	src      []byte
	isTest   bool
	isMain   bool
	enabled  map[string]bool
	findings []model.Finding
	// safeAsserts holds positions of type assertions already guarded by the
	// comma-ok form or a type switch, so the naked-assertion rule can skip them.
	safeAsserts map[token.Pos]bool
	// nolint holds lines the author has explicitly excused.
	nolint map[int]bool
	// verdicts holds quality: annotations by line.
	verdicts map[int]verdict.V
	// config carries pattern-matched judgements from project config.
	config  verdict.Config
	fnStack []string
}

func (p *smellPass) on(rule string) bool { return p.enabled == nil || p.enabled[rule] }

func (p *smellPass) currentFunc() string {
	if len(p.fnStack) == 0 {
		return ""
	}
	return p.fnStack[len(p.fnStack)-1]
}

func (p *smellPass) snippet(n ast.Node) string {
	s, e := p.fset.Position(n.Pos()).Offset, p.fset.Position(n.End()).Offset
	if s < 0 || e > len(p.src) || s >= e {
		return ""
	}
	if e-s > 200 {
		e = s + 200
	}
	return string(p.src[s:e])
}

// collectNolint records lines carrying a //nolint directive.
//
// This codebase (and most Go codebases) already runs golangci-lint, so
// //nolint is the established way people say "I know, and I meant it". Ignoring
// it would make ratchet the one tool that cannot be told no — which is the
// fastest route to being switched off entirely.
//
// Approximation: a directive suppresses its own line and the line after it,
// covering both the trailing-comment and comment-above-the-declaration forms.
// Full golangci-lint semantics scope to the whole following block; we do not.
func (p *smellPass) collectNolint() {
	p.nolint = map[int]bool{}
	for _, cg := range p.file.Comments {
		for _, c := range cg.List {
			if !strings.Contains(c.Text, "nolint") {
				continue
			}
			line := p.fset.Position(c.Pos()).Line
			p.nolint[line] = true
			p.nolint[line+1] = true
		}
	}
}

func (p *smellPass) add(rule string, n ast.Node, msg, suggest string) {
	if !p.on(rule) {
		return
	}
	pos := p.fset.Position(n.Pos())
	if p.nolint[pos.Line] {
		return
	}
	fn := p.currentFunc()

	// Resolve a judgement. An in-code annotation beats project config: it is
	// more specific, and someone wrote it while looking at this exact code.
	var v verdict.V
	var judged bool
	if vv, ok := p.verdicts[pos.Line]; ok {
		v, judged = vv, true
	} else if vv, ok := p.config.Match(rule, p.relPath); ok {
		v, judged = vv, true
	}
	f := model.Finding{
		Rule:        rule,
		Severity:    Rules[rule].Severity,
		File:        p.relPath,
		Line:        pos.Line,
		Col:         pos.Column,
		Func:        fn,
		Message:     msg,
		Suggest:     suggest,
		Fingerprint: model.Fingerprint(p.relPath, rule, fn, p.snippet(n)),
	}
	if judged {
		verdict.Apply(&f, v)
	}
	p.findings = append(p.findings, f)
}

// collectSafeAsserts pre-walks the file to record every type assertion that is
// already guarded. Two forms are safe:
//
//	v, ok := x.(T)      — comma-ok, caller handles the miss
//	switch x.(type)     — type switch, exhaustive by construction
//
// Anything left over panics on a wrong type, which is the pattern worth flagging.
func (p *smellPass) collectSafeAsserts() {
	p.safeAsserts = map[token.Pos]bool{}
	ast.Inspect(p.file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 2 && len(s.Rhs) == 1 {
				if ta, ok := s.Rhs[0].(*ast.TypeAssertExpr); ok {
					p.safeAsserts[ta.Pos()] = true
				}
			}
		case *ast.TypeSwitchStmt:
			ast.Inspect(s.Assign, func(m ast.Node) bool {
				if ta, ok := m.(*ast.TypeAssertExpr); ok {
					p.safeAsserts[ta.Pos()] = true
				}
				return true
			})
		}
		return true
	})
}

func (p *smellPass) run() []model.Finding {
	p.collectSafeAsserts()
	p.collectNolint()
	p.verdicts = verdict.FromComments(p.fset, p.file)

	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch s := n.(type) {

		case *ast.FuncDecl:
			p.fnStack = append(p.fnStack, funcName(s))
			defer func() { p.fnStack = p.fnStack[:len(p.fnStack)-1] }()
			p.checkExportedAny(s)
			ast.Inspect(s, func(m ast.Node) bool {
				if m == n {
					return true
				}
				return visit(m)
			})
			return false

		case *ast.TypeAssertExpr:
			// Type is nil for the `x.(type)` form inside a type switch guard.
			if s.Type != nil && !p.safeAsserts[s.Pos()] {
				p.add(RuleNakedAssert, s,
					"type assertion without comma-ok will panic if the type does not match",
					"use `v, ok := x.(T)` and handle !ok")
			}

		case *ast.AssignStmt:
			p.checkErrSwallow(s)

		case *ast.IfStmt:
			p.checkEmptyErrBranch(s)
			p.checkElseAfterJump(s)

		case *ast.CallExpr:
			p.checkPanic(s)
		}
		return true
	}
	ast.Inspect(p.file, visit)
	return p.findings
}

// checkErrSwallow catches `_ = err` — the assignment exists only to silence the
// compiler, which means a failure path was acknowledged and then dropped.
func (p *smellPass) checkErrSwallow(s *ast.AssignStmt) {
	for i, lhs := range s.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name != "_" || i >= len(s.Rhs) {
			continue
		}
		if rid, ok := s.Rhs[i].(*ast.Ident); ok && looksLikeErr(rid.Name) {
			p.add(RuleErrSwallowed, s,
				fmt.Sprintf("error value %q assigned to _ and discarded", rid.Name),
				"handle the error, or document why it is safe to ignore")
		}
	}
}

// checkEmptyErrBranch catches `if err != nil {}` and `if err != nil { return nil }`,
// both of which detect a failure and then pretend it did not happen.
func (p *smellPass) checkEmptyErrBranch(s *ast.IfStmt) {
	be, ok := s.Cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return
	}
	id, ok := be.X.(*ast.Ident)
	if !ok || !looksLikeErr(id.Name) {
		return
	}
	if nid, ok := be.Y.(*ast.Ident); !ok || nid.Name != "nil" {
		return
	}
	if s.Body == nil {
		return
	}
	switch len(s.Body.List) {
	case 0:
		p.add(RuleErrSwallowed, s, "error checked then ignored: empty branch body",
			"handle the error or drop the check")
	case 1:
		ret, ok := s.Body.List[0].(*ast.ReturnStmt)
		if !ok {
			return
		}
		// `return nil` in a branch entered because err != nil discards the error.
		for _, r := range ret.Results {
			if id, ok := r.(*ast.Ident); ok && id.Name == "nil" && len(ret.Results) == 1 {
				p.add(RuleErrSwallowed, s, "error is non-nil but the function returns nil",
					"return the error, or wrap it with context")
			}
		}
	}
}

func (p *smellPass) checkElseAfterJump(s *ast.IfStmt) {
	if s.Else == nil || s.Body == nil || len(s.Body.List) == 0 {
		return
	}
	switch last := s.Body.List[len(s.Body.List)-1].(type) {
	case *ast.ReturnStmt:
	case *ast.BranchStmt:
		if last.Tok != token.BREAK && last.Tok != token.CONTINUE {
			return
		}
	default:
		return
	}
	p.add(RuleElseAfterJump, s.Else,
		"else follows a branch that already returns or jumps",
		"drop the else and outdent its body")
}

func (p *smellPass) checkPanic(c *ast.CallExpr) {
	if p.isTest || p.isMain {
		return
	}
	id, ok := c.Fun.(*ast.Ident)
	if !ok || id.Name != "panic" {
		return
	}
	// The must* prefix is an established Go convention announcing that this
	// function panics by design — regexp.MustCompile, template.Must. Flagging
	// it means flagging the stdlib's own idiom.
	if fn := p.currentFunc(); strings.HasPrefix(fn, "must") || strings.HasPrefix(fn, "Must") {
		return
	}
	p.add(RulePanicInLib, c,
		"panic() in library code takes the decision away from the caller",
		"return an error instead")
}

func (p *smellPass) checkExportedAny(fd *ast.FuncDecl) {
	if !fd.Name.IsExported() || fd.Type == nil {
		return
	}
	report := func(f *ast.Field, where string) {
		// Variadic ...any is the pass-through idiom: fmt.Printf, SQL driver
		// args, structured logging. It dominates the legitimate uses, so
		// flagging it produces noise that buries the real findings.
		if _, variadic := f.Type.(*ast.Ellipsis); variadic {
			return
		}
		if isAny(f.Type) {
			p.add(RuleAnyExported, f,
				fmt.Sprintf("exported %s uses any/interface{} in %s", funcName(fd), where),
				"use a concrete type or a named interface that states the requirement")
		}
	}
	if fd.Type.Params != nil {
		for _, f := range fd.Type.Params.List {
			report(f, "a parameter")
		}
	}
	if fd.Type.Results != nil {
		for _, f := range fd.Type.Results.List {
			report(f, "a result")
		}
	}
}

func isAny(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.InterfaceType:
		return t.Methods == nil || len(t.Methods.List) == 0
	case *ast.Ident:
		return t.Name == "any"
	case *ast.Ellipsis:
		return isAny(t.Elt)
	case *ast.ArrayType:
		return isAny(t.Elt)
	}
	return false
}

// looksLikeErr is a heuristic: ratchet parses without type information so it
// cannot ask whether something implements error. Name-based matching keeps the
// tool dependency-free and fast enough to replay over history, at the cost of
// missing errors bound to unconventional names. Worth revisiting if we ever
// take the go/types dependency.
func looksLikeErr(n string) bool {
	return n == "err" || n == "e" || n == "error" ||
		(len(n) > 3 && (n[:3] == "err" || n[len(n)-3:] == "Err"))
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return recvTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}
