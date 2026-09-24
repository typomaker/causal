package causal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
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
type compiledCase struct {
	name string
	code []instruction
	deps map[string]struct{}
}

// Engine is an immutable compiled schema and is safe for concurrent use.
type Engine struct {
	env        *cel.Env
	cases      map[string]*compiledCase
	funcs      map[string]*funcDef
	dependents map[string][]string
	version    string
}

func Compile(schema Program) (*Engine, error) {
	funcs := map[string]*funcDef{}
	caseDefs := map[string]*caseDef{}
	var roots []*caseDef
	for _, item := range schema.items {
		switch x := item.(type) {
		case *funcDef:
			if err := validateFunc(x); err != nil {
				return nil, err
			}
			if _, exists := funcs[x.name]; exists {
				return nil, fmt.Errorf("causal: duplicate function %q", x.name)
			}
			funcs[x.name] = x
		case *caseDef:
			roots = append(roots, x)
		default:
			return nil, fmt.Errorf("causal: unsupported schema item %T", item)
		}
	}
	var collect func(*caseDef) error
	collect = func(c *caseDef) error {
		if c.name == "" {
			return fmt.Errorf("causal: case name is empty")
		}
		if old, ok := caseDefs[c.name]; ok && old != c {
			return fmt.Errorf("causal: duplicate case %q", c.name)
		}
		caseDefs[c.name] = c
		for _, op := range c.ops {
			if child, ok := op.(*caseDef); ok {
				if err := collect(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Resolve named structural references only after all composed declarations
	// have been collected. This permits references across JSON documents.
	var resolve func(*caseDef, []string) error
	resolve = func(c *caseDef, path []string) error {
		for i, raw := range c.ops {
			ref, ok := raw.(caseRefOp)
			if !ok {
				continue
			}
			target, exists := caseDefs[ref.name]
			if !exists {
				return fmt.Errorf("causal: case %q: unknown case %q", c.name, ref.name)
			}
			cycleAt := -1
			for j, name := range path {
				if name == ref.name {
					cycleAt = j
					break
				}
			}
			if cycleAt >= 0 {
				cycle := append(append([]string{}, path[cycleAt:]...), ref.name)
				return fmt.Errorf("causal: recursive case reference: %s", strings.Join(cycle, " -> "))
			}
			if err := resolve(target, append(path, ref.name)); err != nil {
				return err
			}
			c.ops[i] = resolvedCaseRef{target}
		}
		return nil
	}
	for _, c := range roots {
		if err := collect(c); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].name < roots[j].name })
	for _, c := range roots {
		if err := resolve(c, []string{c.name}); err != nil {
			return nil, err
		}
	}
	if len(caseDefs) == 0 {
		return nil, fmt.Errorf("causal: schema has no cases")
	}

	// Parse first with a permissive environment to discover state identifiers.
	parseEnv, _ := cel.NewEnv()
	identifiers := map[string]struct{}{}
	var sources []string
	var gatherSources func(*caseDef)
	gatherSources = func(c *caseDef) {
		for _, op := range c.ops {
			switch x := op.(type) {
			case withOp:
				sources = append(sources, x.expr)
			case skipOp:
				sources = append(sources, x.expr)
			case waitOp:
				sources = append(sources, x.expr)
			case *caseDef:
				gatherSources(x)
			case resolvedCaseRef:
				gatherSources(x.def)
			}
		}
	}
	for _, c := range roots {
		gatherSources(c)
	}
	for _, src := range sources {
		ast, iss := parseEnv.Parse(src)
		if iss.Err() != nil {
			return nil, fmt.Errorf("causal: parse %q: %w", src, iss.Err())
		}
		collectIdentifiers(ast.Expr(), nil, identifiers)
	}
	for name := range funcs {
		delete(identifiers, name)
	}
	opts := []cel.EnvOption{}
	ids := make([]string, 0, len(identifiers))
	for id := range identifiers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		opts = append(opts, cel.Variable(id, cel.DynType))
	}
	for _, f := range funcs {
		opts = append(opts, cel.Function(f.name, cel.Overload(overloadID(f.name), f.args, f.result)))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	e := &Engine{env: env, cases: map[string]*compiledCase{}, funcs: funcs, dependents: map[string][]string{}, version: schemaVersion(schema)}
	for _, def := range roots {
		name := def.name
		cc := &compiledCase{name: name, deps: map[string]struct{}{}}
		if _, err := compileOps(env, def, &cc.code, cc.deps, ""); err != nil {
			return nil, fmt.Errorf("causal: case %q: %w", name, err)
		}
		e.cases[name] = cc
		for dep := range cc.deps {
			e.dependents[dep] = append(e.dependents[dep], name)
		}
	}
	return e, nil
}

func compileOps(env *cel.Env, c *caseDef, code *[]instruction, deps map[string]struct{}, self string) (string, error) {
	for _, raw := range c.ops {
		switch op := raw.(type) {
		case selfOp:
			if op.name == "" {
				return self, fmt.Errorf("Self name is empty")
			}
			self = op.name
		case withOp:
			if self == "" {
				return self, fmt.Errorf("With %q has no preceding Self", op.expr)
			}
			ast, err := compileExpr(env, op.expr)
			if err != nil {
				return self, err
			}
			addDeps(ast, deps)
			deps[self] = struct{}{}
			*code = append(*code, instruction{kind: instWith, target: self, source: op.expr, ast: ast})
		case skipOp:
			ast, err := compileExpr(env, op.expr)
			if err != nil {
				return self, err
			}
			if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
				return self, fmt.Errorf("Skip %q must return bool, got %v", op.expr, ast.OutputType())
			}
			addDeps(ast, deps)
			at := len(*code)
			*code = append(*code, instruction{kind: instSkip, source: op.expr, ast: ast})
			(*code)[at].jump = -1
		case waitOp:
			ast, err := compileExpr(env, op.expr)
			if err != nil {
				return self, err
			}
			if ast.OutputType() != cel.DurationType && ast.OutputType() != cel.DynType {
				return self, fmt.Errorf("Wait %q must return duration, got %v", op.expr, ast.OutputType())
			}
			addDeps(ast, deps)
			*code = append(*code, instruction{kind: instWait, source: op.expr, ast: ast})
		case *caseDef:
			start := len(*code)
			var err error
			self, err = compileOps(env, op, code, deps, self)
			if err != nil {
				return self, err
			}
			end := len(*code)
			*code = append(*code, instruction{kind: instEndCase})
			for i := start; i < end; i++ {
				if (*code)[i].kind == instSkip && (*code)[i].jump == -1 {
					(*code)[i].jump = end
				}
			}
		case resolvedCaseRef:
			start := len(*code)
			var err error
			self, err = compileOps(env, op.def, code, deps, self)
			if err != nil {
				return self, err
			}
			end := len(*code)
			*code = append(*code, instruction{kind: instEndCase})
			for i := start; i < end; i++ {
				if (*code)[i].kind == instSkip && (*code)[i].jump == -1 {
					(*code)[i].jump = end
				}
			}
		default:
			return self, fmt.Errorf("unsupported operation %T", raw)
		}
	}
	end := len(*code)
	*code = append(*code, instruction{kind: instEndCase})
	for i := 0; i < end; i++ {
		if (*code)[i].kind == instSkip && (*code)[i].jump == -1 {
			(*code)[i].jump = end
		}
	}
	return self, nil
}

// MarshalJSON emits non-executable Engine metadata only.
func (e *Engine) MarshalJSON() ([]byte, error) {
	names := make([]string, 0, len(e.cases))
	for name := range e.cases {
		names = append(names, name)
	}
	sort.Strings(names)
	return json.Marshal(struct {
		Version string   `json:"version"`
		Cases   []string `json:"cases"`
	}{e.version, names})
}

func schemaVersion(s Program) string {
	h := sha256.New()
	var walk func(*caseDef)
	walk = func(c *caseDef) {
		fmt.Fprintf(h, "case:%s{", c.name)
		for _, raw := range c.ops {
			switch x := raw.(type) {
			case selfOp:
				fmt.Fprintf(h, "self:%s;", x.name)
			case withOp:
				fmt.Fprintf(h, "with:%s;", x.expr)
			case skipOp:
				fmt.Fprintf(h, "skip:%s;", x.expr)
			case waitOp:
				fmt.Fprintf(h, "wait:%s;", x.expr)
			case *caseDef:
				walk(x)
			case caseRefOp:
				fmt.Fprintf(h, "case-ref:%s;", x.name)
			case resolvedCaseRef:
				fmt.Fprintf(h, "case-ref:%s;", x.def.name)
			}
		}
		fmt.Fprint(h, "}")
	}
	for _, item := range s.items {
		switch x := item.(type) {
		case *funcDef:
			fmt.Fprintf(h, "func:%s:%s;", x.name, x.signature)
		case *caseDef:
			walk(x)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func compileExpr(env *cel.Env, src string) (*cel.Ast, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("empty CEL expression")
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", src, iss.Err())
	}
	return ast, nil
}
func addDeps(ast *cel.Ast, out map[string]struct{}) { collectIdentifiers(ast.Expr(), nil, out) }
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
		for _, ent := range x.StructExpr.Entries {
			collectIdentifiers(ent.Value, locals, out)
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
func overloadID(name string) string { return "causal_" + name }
func celError(err error) ref.Val    { return types.NewErr("%s", err) }
