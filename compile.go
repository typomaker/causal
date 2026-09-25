package causal

import (
	"causal/ast"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"sort"
	"strings"
)

type instructionKind uint8

const (
	instWith instructionKind = iota
	instSkip
	instWait
	instEndCase
)

type instruction struct {
	kind           instructionKind
	target, source string
	ast            *cel.Ast
	jump           int
}
type bindingRequirement struct{ write, function bool }
type compiledCase struct {
	name         string
	code         []instruction
	deps         map[string]struct{}
	requirements map[string]bindingRequirement
}

// Runtime is an immutable compiled program and is safe for concurrent use.
type Runtime struct {
	env        *cel.Env
	cases      map[string]*compiledCase
	contracts  map[string]symbolContract
	dependents map[string][]string
	version    string
}

func compile(program Program) (*Runtime, error) {
	contracts := map[string]symbolContract{}
	for _, c := range program.contracts {
		if c.err != nil {
			return nil, c.err
		}
		if c.symbol.Name == "" {
			return nil, fmt.Errorf("causal: symbol name is empty")
		}
		if _, ok := contracts[c.symbol.Name]; ok {
			return nil, fmt.Errorf("causal: duplicate symbol %q", c.symbol.Name)
		}
		contracts[c.symbol.Name] = c
	}
	defs := map[string]ast.Case{}
	var collect func(ast.Case) error
	collect = func(c ast.Case) error {
		if c.Name == "" {
			return fmt.Errorf("causal: case name is empty")
		}
		if _, ok := defs[c.Name]; ok {
			return fmt.Errorf("causal: duplicate case %q", c.Name)
		}
		defs[c.Name] = c
		for _, s := range c.Statements {
			if child, ok := s.(ast.Case); ok {
				if err := collect(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, c := range program.AST.Cases {
		if err := collect(c); err != nil {
			return nil, err
		}
	}
	if len(program.AST.Cases) == 0 {
		return nil, fmt.Errorf("causal: program has no cases")
	}
	opts := []cel.EnvOption{cel.CrossTypeNumericComparisons(true), maxFunction()}
	for _, name := range sortedContracts(contracts) {
		c := contracts[name]
		if c.function {
			args := make([]*cel.Type, len(c.args))
			for i, k := range c.args {
				args[i] = celType(k)
			}
			opts = append(opts, cel.Function(name, cel.Overload(overloadID(name), args, celType(c.result))))
		} else {
			opts = append(opts, cel.Variable(name, celType(c.kind)))
		}
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	r := &Runtime{env: env, cases: map[string]*compiledCase{}, contracts: contracts, dependents: map[string][]string{}, version: programVersion(program)}
	for _, def := range program.AST.Cases {
		cc := &compiledCase{name: def.Name, deps: map[string]struct{}{}, requirements: map[string]bindingRequirement{}}
		if _, err := compileStatements(env, def, defs, &cc.code, cc.deps, cc.requirements, "", []string{def.Name}); err != nil {
			return nil, fmt.Errorf("causal: case %q: %w", def.Name, err)
		}
		for dep := range cc.deps {
			c, ok := contracts[dep]
			if !ok || c.function {
				return nil, fmt.Errorf("causal: case %q: undeclared value symbol %q", def.Name, dep)
			}
			q := cc.requirements[dep]
			cc.requirements[dep] = q
			r.dependents[dep] = append(r.dependents[dep], def.Name)
		}
		for name, c := range contracts {
			if c.function && usesFunction(cc.code, name) {
				cc.requirements[name] = bindingRequirement{function: true}
			}
		}
		r.cases[def.Name] = cc
	}
	return r, nil
}

func compileStatements(env *cel.Env, c ast.Case, defs map[string]ast.Case, code *[]instruction, deps map[string]struct{}, req map[string]bindingRequirement, self string, path []string) (string, error) {
	start := len(*code)
	for _, raw := range c.Statements {
		switch s := raw.(type) {
		case ast.Self:
			if s.Symbol == "" {
				return self, fmt.Errorf("Self name is empty")
			}
			self = s.Symbol
		case ast.With:
			if self == "" {
				return self, fmt.Errorf("With %q has no preceding Self", s.Expression)
			}
			a, e := compileExpr(env, s.Expression)
			if e != nil {
				return self, e
			}
			addDeps(a, deps)
			deps[self] = struct{}{}
			q := req[self]
			q.write = true
			req[self] = q
			*code = append(*code, instruction{kind: instWith, target: self, source: s.Expression, ast: a})
		case ast.Skip:
			a, e := compileExpr(env, s.Expression)
			if e != nil {
				return self, e
			}
			if a.OutputType() != cel.BoolType {
				return self, fmt.Errorf("Skip %q must return bool, got %v", s.Expression, a.OutputType())
			}
			addDeps(a, deps)
			*code = append(*code, instruction{kind: instSkip, source: s.Expression, ast: a, jump: -1})
		case ast.Wait:
			a, e := compileExpr(env, s.Expression)
			if e != nil {
				return self, e
			}
			if a.OutputType() != cel.DurationType {
				return self, fmt.Errorf("Wait %q must return duration, got %v", s.Expression, a.OutputType())
			}
			addDeps(a, deps)
			*code = append(*code, instruction{kind: instWait, source: s.Expression, ast: a})
		case ast.Case:
			var e error
			self, e = compileStatements(env, s, defs, code, deps, req, self, append(path, s.Name))
			if e != nil {
				return self, e
			}
		case ast.CaseRef:
			target, ok := defs[s.Name]
			if !ok {
				return self, fmt.Errorf("unknown case %q", s.Name)
			}
			for _, n := range path {
				if n == s.Name {
					return self, fmt.Errorf("recursive case reference: %s", strings.Join(append(path, s.Name), " -> "))
				}
			}
			var e error
			self, e = compileStatements(env, target, defs, code, deps, req, self, append(path, s.Name))
			if e != nil {
				return self, e
			}
		default:
			return self, fmt.Errorf("unsupported statement %T", raw)
		}
	}
	end := len(*code)
	*code = append(*code, instruction{kind: instEndCase})
	for i := start; i < end; i++ {
		if (*code)[i].kind == instSkip && (*code)[i].jump == -1 {
			(*code)[i].jump = end
		}
	}
	return self, nil
}

func sortedContracts(m map[string]symbolContract) []string {
	r := make([]string, 0, len(m))
	for n := range m {
		r = append(r, n)
	}
	sort.Strings(r)
	return r
}
func celType(k string) *cel.Type {
	switch k {
	case "bool":
		return cel.BoolType
	case "string":
		return cel.StringType
	case "int":
		return cel.IntType
	case "uint":
		return cel.UintType
	case "double":
		return cel.DoubleType
	case "duration":
		return cel.DurationType
	}
	return cel.DynType
}
func compileExpr(env *cel.Env, src string) (*cel.Ast, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("empty CEL expression")
	}
	a, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", src, iss.Err())
	}
	return a, nil
}
func addDeps(a *cel.Ast, out map[string]struct{}) { collectIdentifiers(a.Expr(), nil, out) }
func collectIdentifiers(e *exprpb.Expr, locals map[string]bool, out map[string]struct{}) {
	if e == nil {
		return
	}
	switch x := e.ExprKind.(type) {
	case *exprpb.Expr_IdentExpr:
		if locals == nil || !locals[x.IdentExpr.Name] {
			out[x.IdentExpr.Name] = struct{}{}
		}
	case *exprpb.Expr_SelectExpr:
		collectIdentifiers(x.SelectExpr.Operand, locals, out)
	case *exprpb.Expr_CallExpr:
		collectIdentifiers(x.CallExpr.Target, locals, out)
		for _, a := range x.CallExpr.Args {
			collectIdentifiers(a, locals, out)
		}
	case *exprpb.Expr_ListExpr:
		for _, a := range x.ListExpr.Elements {
			collectIdentifiers(a, locals, out)
		}
	case *exprpb.Expr_StructExpr:
		for _, a := range x.StructExpr.Entries {
			collectIdentifiers(a.Value, locals, out)
		}
	case *exprpb.Expr_ComprehensionExpr:
		collectIdentifiers(x.ComprehensionExpr.IterRange, locals, out)
		collectIdentifiers(x.ComprehensionExpr.AccuInit, locals, out)
		n := map[string]bool{}
		for k, v := range locals {
			n[k] = v
		}
		n[x.ComprehensionExpr.IterVar] = true
		n[x.ComprehensionExpr.AccuVar] = true
		collectIdentifiers(x.ComprehensionExpr.LoopCondition, n, out)
		collectIdentifiers(x.ComprehensionExpr.LoopStep, n, out)
		collectIdentifiers(x.ComprehensionExpr.Result, n, out)
	}
}
func usesFunction(code []instruction, name string) bool {
	for _, i := range code {
		if i.ast != nil && expressionUsesFunction(i.ast.Expr(), name) {
			return true
		}
	}
	return false
}
func expressionUsesFunction(e *exprpb.Expr, name string) bool {
	if e == nil {
		return false
	}
	switch x := e.ExprKind.(type) {
	case *exprpb.Expr_SelectExpr:
		return expressionUsesFunction(x.SelectExpr.Operand, name)
	case *exprpb.Expr_CallExpr:
		if x.CallExpr.Function == name || expressionUsesFunction(x.CallExpr.Target, name) {
			return true
		}
		for _, a := range x.CallExpr.Args {
			if expressionUsesFunction(a, name) {
				return true
			}
		}
	case *exprpb.Expr_ListExpr:
		for _, a := range x.ListExpr.Elements {
			if expressionUsesFunction(a, name) {
				return true
			}
		}
	case *exprpb.Expr_StructExpr:
		for _, a := range x.StructExpr.Entries {
			if expressionUsesFunction(a.Value, name) {
				return true
			}
		}
	case *exprpb.Expr_ComprehensionExpr:
		return expressionUsesFunction(x.ComprehensionExpr.IterRange, name) || expressionUsesFunction(x.ComprehensionExpr.AccuInit, name) || expressionUsesFunction(x.ComprehensionExpr.LoopCondition, name) || expressionUsesFunction(x.ComprehensionExpr.LoopStep, name) || expressionUsesFunction(x.ComprehensionExpr.Result, name)
	}
	return false
}
func programVersion(p Program) string {
	b, _ := p.MarshalJSON()
	h := sha256.New()
	h.Write(b)
	names := make([]string, 0, len(p.contracts))
	m := map[string]symbolContract{}
	for _, c := range p.contracts {
		names = append(names, c.symbol.Name)
		m[c.symbol.Name] = c
	}
	sort.Strings(names)
	for _, n := range names {
		c := m[n]
		fmt.Fprintf(h, "%s:%s:%v:%s:%v;", n, c.kind, c.args, c.result, c.goType)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func overloadID(name string) string { return "causal_" + name }

func maxFunction() cel.EnvOption {
	max := func(a, b ref.Val) ref.Val {
		av, aok := numeric(a)
		bv, bok := numeric(b)
		if !aok || !bok {
			return types.NewErr("max arguments must be numeric")
		}
		if av > bv {
			return types.Double(av)
		}
		return types.Double(bv)
	}
	return cel.Function("max",
		cel.Overload("causal_max_double_double", []*cel.Type{cel.DoubleType, cel.DoubleType}, cel.DoubleType, cel.BinaryBinding(max)),
		cel.Overload("causal_max_int_double", []*cel.Type{cel.IntType, cel.DoubleType}, cel.DoubleType, cel.BinaryBinding(max)),
		cel.Overload("causal_max_double_int", []*cel.Type{cel.DoubleType, cel.IntType}, cel.DoubleType, cel.BinaryBinding(max)),
	)
}
func numeric(v ref.Val) (float64, bool) {
	switch x := v.Value().(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}
