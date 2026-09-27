package causal

import (
	"causal/ast"
	"context"
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
	"sync"
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
	targetIndex    int
	expr           *compiledExpr
	jump           int
}

// compiledExpr contains all immutable work needed to evaluate one expression.
// In particular, dependency discovery and CEL planning happen during Compile.
type compiledExpr struct {
	reads       []string
	readIndexes []int
	functions   []string
	output      *cel.Type
	program     cel.Program
}
type compiledCase struct {
	name               string
	code               []instruction
	deps               map[string]struct{}
	requirements       map[string]struct{}
	requirementIndexes []int
}

// Runtime is an immutable compiled program and is safe for concurrent use. Use
// [Program.Compile] to create one, then execute named root cases with [Runtime.Do].
//
//	runtime, err := program.Compile()
//	if err == nil {
//		err = runtime.Do(ctx, &scope, "attack", bindings...)
//	}
type Runtime struct {
	cases         map[string]*compiledCase
	symbols       []symbolContract
	symbolIndexes map[string]int
	dependents    [][]string
	maxTargets    int
	segmentPool   sync.Pool
	version       string
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
	names := sortedContracts(contracts)
	symbolIndexes := make(map[string]int, len(names))
	symbols := make([]symbolContract, len(names))
	opts := []cel.EnvOption{cel.CrossTypeNumericComparisons(true), maxFunction()}
	for index, name := range names {
		c := contracts[name]
		symbolIndexes[name] = index
		symbols[index] = c
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
	r := &Runtime{cases: map[string]*compiledCase{}, symbols: symbols, symbolIndexes: symbolIndexes, dependents: make([][]string, len(symbols)), version: programVersion(program, contracts)}
	for _, def := range program.AST.Cases {
		cc := &compiledCase{name: def.Name, deps: map[string]struct{}{}, requirements: map[string]struct{}{}}
		if _, err := compileStatements(env, def, defs, symbolIndexes, &cc.code, cc.deps, cc.requirements, "", []string{def.Name}); err != nil {
			return nil, fmt.Errorf("causal: case %q: %w", def.Name, err)
		}
		for dep := range cc.deps {
			c, ok := contracts[dep]
			if !ok || c.function {
				return nil, fmt.Errorf("causal: case %q: undeclared value symbol %q", def.Name, dep)
			}
			cc.requirements[dep] = struct{}{}
			index := symbolIndexes[dep]
			r.dependents[index] = append(r.dependents[index], def.Name)
		}
		cc.requirementIndexes = make([]int, 0, len(cc.requirements))
		for name := range cc.requirements {
			cc.requirementIndexes = append(cc.requirementIndexes, symbolIndexes[name])
		}
		sort.Ints(cc.requirementIndexes)
		for i := range cc.code {
			ins := &cc.code[i]
			if ins.target != "" {
				ins.targetIndex = symbolIndexes[ins.target]
			}
			if ins.expr != nil {
				ins.expr.readIndexes = make([]int, len(ins.expr.reads))
				for j, name := range ins.expr.reads {
					ins.expr.readIndexes[j] = symbolIndexes[name]
				}
			}
		}
		targets := make(map[int]struct{})
		for _, ins := range cc.code {
			if ins.kind == instWith {
				targets[ins.targetIndex] = struct{}{}
			}
		}
		if len(targets) > r.maxTargets {
			r.maxTargets = len(targets)
		}
		r.cases[def.Name] = cc
	}
	r.segmentPool.New = func() any {
		return &segment{values: make([]segmentValue, len(r.symbols)), order: make([]int, 0, r.maxTargets)}
	}
	return r, nil
}

func compileStatements(env *cel.Env, c ast.Case, defs map[string]ast.Case, symbolIndexes map[string]int, code *[]instruction, deps map[string]struct{}, req map[string]struct{}, self string, path []string) (string, error) {
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
			a, e := compileExprIndexed(env, s.Expression, req, symbolIndexes)
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
			a, e := compileExprIndexed(env, s.Expression, req, symbolIndexes)
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
			a, e := compileExprIndexed(env, s.Expression, req, symbolIndexes)
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
			self, e = compileStatements(env, s, defs, symbolIndexes, code, deps, req, self, append(path, s.Name))
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
			self, e = compileStatements(env, target, defs, symbolIndexes, code, deps, req, self, append(path, s.Name))
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
	if element, ok := listElementKind(k); ok {
		return cel.ListType(celType(element))
	}
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
	return compileExprIndexed(env, src, requirements, nil)
}

func compileExprIndexed(env *cel.Env, src string, requirements map[string]struct{}, symbolIndexes map[string]int) (*compiledExpr, error) {
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
	p, err := env.Program(a, cel.CustomDecorator(dynamicFunctionDecorator(functionSet, symbolIndexes)))
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

func dynamicFunctionDecorator(functions map[string]struct{}, symbolIndexes map[string]int) interpreter.InterpretableDecorator {
	return func(value interpreter.Interpretable) (interpreter.Interpretable, error) {
		call, ok := value.(interpreter.InterpretableCall)
		if !ok {
			return value, nil
		}
		name := call.Function()
		if _, ok := functions[name]; !ok {
			return value, nil
		}
		index, ok := symbolIndexes[name]
		if !ok {
			index = -1
		}
		return &dynamicFunctionCall{id: call.ID(), name: name, index: index, args: call.Args()}, nil
	}
}

type dynamicFunctionCall struct {
	id    int64
	name  string
	index int
	args  []interpreter.Interpretable
}

func (c *dynamicFunctionCall) ID() int64 { return c.id }
func (c *dynamicFunctionCall) Eval(activation interpreter.Activation) ref.Val {
	value, ok := activation.ResolveName(executionActivationName)
	if !ok {
		return types.NewErr("causal: missing function activation")
	}
	execution, ok := value.(*expressionActivation)
	if !ok {
		return types.NewErr("causal: invalid function activation")
	}
	if c.index < 0 || c.index >= len(execution.bindings) || !execution.bindings[c.index].set || execution.bindings[c.index].function == nil {
		return types.NewErr("causal: missing function %s", c.name)
	}
	function := execution.bindings[c.index].function
	if function.typedArity >= 0 {
		return c.evalTyped(execution.context, function, activation)
	}
	args := make([]ref.Val, len(c.args))
	for i, arg := range c.args {
		args[i] = arg.Eval(activation)
		if types.IsUnknownOrError(args[i]) {
			return args[i]
		}
	}
	return function.call(execution.context, args)
}

func (c *dynamicFunctionCall) evalTyped(ctx context.Context, function *boundFunction, activation interpreter.Activation) ref.Val {
	if len(c.args) != function.typedArity || len(c.args) > 8 {
		return types.NewErr("function %s: got %d arguments, want %d", c.name, len(c.args), function.typedArity)
	}
	var args [8]ref.Val
	for i, arg := range c.args {
		args[i] = arg.Eval(activation)
		if types.IsUnknownOrError(args[i]) {
			return args[i]
		}
	}
	switch function.typedArity {
	case 0:
		return function.call0(ctx)
	case 1:
		return function.call1(ctx, args[0])
	case 2:
		return function.call2(ctx, args[0], args[1])
	case 3:
		return function.call3(ctx, args[0], args[1], args[2])
	case 4:
		return function.call4(ctx, args[0], args[1], args[2], args[3])
	case 5:
		return function.call5(ctx, args[0], args[1], args[2], args[3], args[4])
	case 6:
		return function.call6(ctx, args[0], args[1], args[2], args[3], args[4], args[5])
	case 7:
		return function.call7(ctx, args[0], args[1], args[2], args[3], args[4], args[5], args[6])
	case 8:
		return function.call8(ctx, args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7])
	default:
		return types.NewErr("function %s: unsupported typed arity %d", c.name, function.typedArity)
	}
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
