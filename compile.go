package causal

import (
	"causal/ast"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"
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
	expr           *compiledExpr
	jump           int
}

// compiledExpr contains all immutable work needed to evaluate one expression.
// In particular, dependency discovery and CEL planning happen during Compile.
type compiledExpr struct {
	reads     []string
	functions []string
	output    *cel.Type
	program   cel.Program
}
type compiledCase struct {
	name         string
	code         []instruction
	deps         map[string]struct{}
	requirements map[string]struct{}
}

// Runtime is an immutable compiled program and is safe for concurrent use. Use
// [Program.Compile] to create one, then execute named root cases with [Runtime.Do].
//
//	runtime, err := program.Compile()
//	if err == nil {
//		err = runtime.Do(ctx, &scope, "attack", bindings...)
//	}
type Runtime struct {
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
		if existing, ok := contracts[c.symbol.Name]; ok {
			if !sameSymbolSignature(existing, c) {
				return nil, fmt.Errorf("causal: conflicting declarations for symbol %q", c.symbol.Name)
			}
			if existing.goType != nil && c.goType != nil {
				return nil, fmt.Errorf("causal: duplicate Go symbol %q", c.symbol.Name)
			}
			if existing.goType == nil && c.goType != nil {
				contracts[c.symbol.Name] = c
			}
			continue
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
			opts = append(opts, cel.Function(name, cel.Overload(overloadID(name), args, celType(c.result),
				cel.FunctionBinding(func(...ref.Val) ref.Val { return types.NewErr("causal: missing function activation") }))))
		} else {
			opts = append(opts, cel.Variable(name, celType(c.kind)))
		}
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	r := &Runtime{cases: map[string]*compiledCase{}, contracts: contracts, dependents: map[string][]string{}, version: programVersion(program, contracts)}
	for _, def := range program.AST.Cases {
		cc := &compiledCase{name: def.Name, deps: map[string]struct{}{}, requirements: map[string]struct{}{}}
		if _, err := compileStatements(env, def, defs, &cc.code, cc.deps, cc.requirements, "", []string{def.Name}); err != nil {
			return nil, fmt.Errorf("causal: case %q: %w", def.Name, err)
		}
		for dep := range cc.deps {
			c, ok := contracts[dep]
			if !ok || c.function {
				return nil, fmt.Errorf("causal: case %q: undeclared value symbol %q", def.Name, dep)
			}
			cc.requirements[dep] = struct{}{}
			r.dependents[dep] = append(r.dependents[dep], def.Name)
		}
		r.cases[def.Name] = cc
	}
	return r, nil
}

func compileStatements(env *cel.Env, c ast.Case, defs map[string]ast.Case, code *[]instruction, deps map[string]struct{}, req map[string]struct{}, self string, path []string) (string, error) {
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
			a, e := compileExpr(env, s.Expression, req)
			if e != nil {
				return self, e
			}
			for _, name := range a.reads {
				deps[name] = struct{}{}
			}
			deps[self] = struct{}{}
			req[self] = struct{}{}
			*code = append(*code, instruction{kind: instWith, target: self, source: s.Expression, expr: a})
		case ast.Skip:
			a, e := compileExpr(env, s.Expression, req)
			if e != nil {
				return self, e
			}
			if outputType(a) != cel.BoolType {
				return self, fmt.Errorf("Skip %q must return bool, got %v", s.Expression, outputType(a))
			}
			for _, name := range a.reads {
				deps[name] = struct{}{}
			}
			*code = append(*code, instruction{kind: instSkip, source: s.Expression, expr: a, jump: -1})
		case ast.Wait:
			a, e := compileExpr(env, s.Expression, req)
			if e != nil {
				return self, e
			}
			if outputType(a) != cel.DurationType {
				return self, fmt.Errorf("Wait %q must return duration, got %v", s.Expression, outputType(a))
			}
			for _, name := range a.reads {
				deps[name] = struct{}{}
			}
			*code = append(*code, instruction{kind: instWait, source: s.Expression, expr: a})
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

const executionActivationName = "@causal.execution"

func compileExpr(env *cel.Env, src string, requirements map[string]struct{}) (*compiledExpr, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("empty CEL expression")
	}
	a, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", src, iss.Err())
	}
	checked, err := cel.AstToCheckedExpr(a)
	if err != nil {
		return nil, err
	}
	readSet := map[string]struct{}{}
	functionSet := map[string]struct{}{}
	collectExpressionMetadata(checked.GetExpr(), checked.GetReferenceMap(), nil, readSet, functionSet)
	reads := sortedSet(readSet)
	functions := sortedSet(functionSet)
	for _, name := range functions {
		requirements[name] = struct{}{}
	}
	p, err := env.Program(a, cel.CustomDecorator(dynamicFunctionDecorator(functionSet)))
	if err != nil {
		return nil, err
	}
	return &compiledExpr{reads: reads, functions: functions, output: a.OutputType(), program: p}, nil
}

func outputType(expr *compiledExpr) *cel.Type { return expr.output }

func sortedSet(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func collectExpressionMetadata(e *exprpb.Expr, refs map[int64]*exprpb.Reference, locals map[string]bool, reads, functions map[string]struct{}) {
	if e == nil {
		return
	}
	switch x := e.ExprKind.(type) {
	case *exprpb.Expr_IdentExpr:
		if locals == nil || !locals[x.IdentExpr.Name] {
			reads[x.IdentExpr.Name] = struct{}{}
		}
	case *exprpb.Expr_SelectExpr:
		collectExpressionMetadata(x.SelectExpr.Operand, refs, locals, reads, functions)
	case *exprpb.Expr_CallExpr:
		for _, overload := range refs[e.Id].GetOverloadId() {
			if overload == overloadID(x.CallExpr.Function) {
				functions[x.CallExpr.Function] = struct{}{}
			}
		}
		collectExpressionMetadata(x.CallExpr.Target, refs, locals, reads, functions)
		for _, arg := range x.CallExpr.Args {
			collectExpressionMetadata(arg, refs, locals, reads, functions)
		}
	case *exprpb.Expr_ListExpr:
		for _, item := range x.ListExpr.Elements {
			collectExpressionMetadata(item, refs, locals, reads, functions)
		}
	case *exprpb.Expr_StructExpr:
		for _, entry := range x.StructExpr.Entries {
			collectExpressionMetadata(entry.Value, refs, locals, reads, functions)
		}
	case *exprpb.Expr_ComprehensionExpr:
		collectExpressionMetadata(x.ComprehensionExpr.IterRange, refs, locals, reads, functions)
		collectExpressionMetadata(x.ComprehensionExpr.AccuInit, refs, locals, reads, functions)
		next := make(map[string]bool, len(locals)+2)
		for name, local := range locals {
			next[name] = local
		}
		next[x.ComprehensionExpr.IterVar], next[x.ComprehensionExpr.AccuVar] = true, true
		collectExpressionMetadata(x.ComprehensionExpr.LoopCondition, refs, next, reads, functions)
		collectExpressionMetadata(x.ComprehensionExpr.LoopStep, refs, next, reads, functions)
		collectExpressionMetadata(x.ComprehensionExpr.Result, refs, next, reads, functions)
	}
}

func dynamicFunctionDecorator(functions map[string]struct{}) interpreter.InterpretableDecorator {
	return func(value interpreter.Interpretable) (interpreter.Interpretable, error) {
		call, ok := value.(interpreter.InterpretableCall)
		if !ok {
			return value, nil
		}
		name := call.Function()
		if _, ok := functions[name]; !ok {
			return value, nil
		}
		return &dynamicFunctionCall{id: call.ID(), name: name, args: call.Args()}, nil
	}
}

type dynamicFunctionCall struct {
	id   int64
	name string
	args []interpreter.Interpretable
}

func (c *dynamicFunctionCall) ID() int64 { return c.id }
func (c *dynamicFunctionCall) Eval(activation interpreter.Activation) ref.Val {
	value, ok := activation.ResolveName(executionActivationName)
	if !ok {
		return types.NewErr("causal: missing function activation")
	}
	execution, ok := value.(*expressionExecution)
	if !ok {
		return types.NewErr("causal: invalid function activation")
	}
	call, ok := execution.functions[c.name]
	if !ok {
		return types.NewErr("causal: missing function %s", c.name)
	}
	args := make([]ref.Val, len(c.args))
	for i, arg := range c.args {
		args[i] = arg.Eval(activation)
		if types.IsUnknownOrError(args[i]) {
			return args[i]
		}
	}
	return call(execution.context, args)
}
func sameSymbolSignature(a, b symbolContract) bool {
	if a.function != b.function {
		return false
	}
	if a.function {
		return a.result == b.result && sameKinds(a.args, b.args)
	}
	return a.kind == b.kind
}

func programVersion(p Program, contracts map[string]symbolContract) string {
	b, _ := p.MarshalJSON()
	h := sha256.New()
	h.Write(b)
	names := make([]string, 0, len(contracts))
	for name := range contracts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		c := contracts[n]
		_, _ = fmt.Fprintf(h, "%s:%s:%v:%s:%v;", n, c.kind, c.args, c.result, c.goType)
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
