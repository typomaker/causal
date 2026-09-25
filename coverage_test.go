package causal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"causal/ast"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

func TestStaticKindsAndContracts(t *testing.T) {
	decls := []Declaration{Symbol[bool]("b"), Symbol[string]("s"), Symbol[int64]("i"), Symbol[uint64]("u"), Symbol[float64]("f"), Symbol[time.Duration]("d"), Symbol[struct{}]("bad")}
	for i, d := range decls[:6] {
		if d.(symbolDeclaration).contract.err != nil {
			t.Fatal(i)
		}
	}
	if decls[6].(symbolDeclaration).contract.err == nil {
		t.Fatal("unsupported type")
	}
	if valueKind[bool]() != "bool" || valueKind[string]() != "string" || valueKind[uint64]() != "uint" || valueKind[time.Duration]() != "duration" || valueKind[struct{}]() == "" {
		t.Fatal("kinds")
	}
}

func TestAllFunctionAdapters(t *testing.T) {
	ctx := context.Background()
	i := types.Int(2)
	f := types.Double(2)
	tests := []struct {
		decl Declaration
		fn   any
		args []ref.Val
	}{
		{Symbol[func(int64) int64]("x"), func(x int64) int64 { return x + 1 }, []ref.Val{i}},
		{Symbol[func(context.Context, int64) int64]("x"), func(context.Context, int64) int64 { return 3 }, []ref.Val{i}},
		{Symbol[func(int64) (int64, error)]("x"), func(x int64) (int64, error) { return x, nil }, []ref.Val{i}},
		{Symbol[func(context.Context, int64) (int64, error)]("x"), func(context.Context, int64) (int64, error) { return 2, nil }, []ref.Val{i}},
		{Symbol[func(int64, int64) int64]("x"), func(x, y int64) int64 { return x + y }, []ref.Val{i, i}},
		{Symbol[func(context.Context, int64, int64) int64]("x"), func(context.Context, int64, int64) int64 { return 4 }, []ref.Val{i, i}},
		{Symbol[func(int64, int64) (int64, error)]("x"), func(x, y int64) (int64, error) { return x + y, nil }, []ref.Val{i, i}},
		{Symbol[func(context.Context, int64, int64) (int64, error)]("x"), func(context.Context, int64, int64) (int64, error) { return 4, nil }, []ref.Val{i, i}},
		{Symbol[func(float64, float64) float64]("x"), func(x, y float64) float64 { return x + y }, []ref.Val{f, f}},
		{Symbol[func(context.Context, float64, float64) float64]("x"), func(context.Context, float64, float64) float64 { return 4 }, []ref.Val{f, f}},
		{Symbol[func(float64, float64) (float64, error)]("x"), func(x, y float64) (float64, error) { return x + y, nil }, []ref.Val{f, f}},
		{Symbol[func(context.Context, float64, float64) (float64, error)]("x"), func(context.Context, float64, float64) (float64, error) { return 4, nil }, []ref.Val{f, f}},
	}
	for n, tt := range tests {
		c := tt.decl.(symbolDeclaration).contract
		call, err := functionAdapter(c, tt.fn)
		if err != nil {
			t.Fatal(n, err)
		}
		if types.IsError(call(ctx, tt.args)) {
			t.Fatal(n)
		}
	}
	c := Symbol[func(int64) int64]("x").(symbolDeclaration).contract
	if _, err := functionAdapter(c, func(float64, float64) float64 { return 0 }); err == nil {
		t.Fatal("accepted mismatch")
	}
	c = Symbol[func(int64) (int64, error)]("x").(symbolDeclaration).contract
	call, _ := functionAdapter(c, func(int64) (int64, error) { return 0, errors.New("bad") })
	if !types.IsError(call(ctx, []ref.Val{types.Int(1)})) {
		t.Fatal("lost error")
	}
}

func TestNestedCasesReadinessAndFailures(t *testing.T) {
	p := New(Symbol[int64]("v"), Case("root", Case("child", Self("v"), Skip("true"), With("v+100")), With("v+1")), Case("writer", Self("v"), With("v+1")))
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	v := int64(1)
	b := Bind("v", Getter(func(context.Context) int64 { return v }), Setter(func(_ context.Context, x int64) { v = x }))
	var s Scope
	if err := r.Do(context.Background(), &s, "root", b); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatal(v)
	}
	if err := r.Do(context.Background(), &s, "writer", b); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(&s)
	if len(data) == 0 {
		t.Fatal()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(r.Do(ctx, &s, "root", b), context.Canceled) {
		t.Fatal("cancel")
	}
}

func TestASTReferenceAndCompileBranches(t *testing.T) {
	p := Program{AST: ast.Program{Cases: []ast.Case{{Name: "x", Statements: []ast.Stmt{ast.CaseRef{Name: "y"}}}, {Name: "y"}}}}
	if _, err := p.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Program{{AST: ast.Program{Cases: []ast.Case{{Name: "x", Statements: []ast.Stmt{ast.CaseRef{Name: "missing"}}}}}}, {AST: ast.Program{Cases: []ast.Case{{Name: "x", Statements: []ast.Stmt{ast.CaseRef{Name: "x"}}}}}}, {AST: ast.Program{Cases: []ast.Case{{Name: "", Statements: nil}}}}} {
		if _, err := bad.Compile(); err == nil {
			t.Fatal("compiled bad")
		}
	}
}

func TestHelperAndErrorBranches(t *testing.T) {
	for _, k := range []string{"bool", "string", "int", "uint", "double", "duration", "other"} {
		if celType(k) == nil {
			t.Fatal(k)
		}
	}
	env, _ := cel.NewEnv(cel.Variable("xs", cel.ListType(cel.IntType)), cel.Variable("m", cel.MapType(cel.StringType, cel.IntType)))
	for _, src := range []string{`xs.map(x, x+1)`, `m.a`, `{1: 2}`, `[1,2]`} {
		a, iss := env.Compile(src)
		if iss.Err() != nil {
			t.Fatal(iss.Err())
		}
		ids := map[string]struct{}{}
		addDeps(a, ids)
		_ = expressionUsesFunction(a.Expr(), "missing")
	}
	p := New(Symbol[int64]("v"), Case("x", Self("v"), With("v+1")))
	r, _ := p.Compile()
	b := Bind("v", Getter(func(context.Context) int64 { return 1 }), Setter(func(context.Context, int64) {}))
	var s Scope
	if err := r.Do(context.Background(), &s, "missing", b); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", b, b); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", Bind("", Getter(func(context.Context) int64 { return 1 }))); err == nil {
		t.Fatal()
	}
	bad := Bind("v", Getter(func(context.Context) int64 { return 1 }), Getter(func(context.Context) float64 { return 1 }))
	if err := r.Do(context.Background(), &s, "x", bad); err == nil {
		t.Fatal()
	}
	_ = s.Clock()
	s.SetClock(nil)
	s.runtimeVersion = "wrong"
	if err := r.Do(context.Background(), &s, "x", b); err == nil {
		t.Fatal()
	}
}

func TestFunctionContractValidation(t *testing.T) {
	bad := []Declaration{
		Symbol[func(...int64) int64]("f"),
		Symbol[func(int) int64]("f"),
		Symbol[func()]("f"),
		Symbol[func() (int64, string)]("f"),
		Symbol[func() (int, error)]("f"),
	}
	for _, declaration := range bad {
		if declaration.(symbolDeclaration).contract.err == nil {
			t.Fatal("accepted invalid function contract")
		}
	}
	var nilFn func() bool
	c := Symbol[func() bool]("f").(symbolDeclaration).contract
	if _, err := functionAdapter(c, nilFn); err == nil {
		t.Fatal("accepted nil function")
	}
	for _, kind := range []string{"bool", "string", "int", "uint", "double", "duration"} {
		if !validKind(kind) {
			t.Fatal(kind)
		}
	}
	if validKind("bad") {
		t.Fatal()
	}
	for _, symbol := range []ast.Symbol{{}, {Name: "x", Type: "bad"}, {Name: "f", Function: true, Arguments: []string{"bad"}, Result: "int"}, {Name: "f", Function: true, Result: "bad"}} {
		if _, err := contractFromAST(symbol); err == nil {
			t.Fatalf("accepted %#v", symbol)
		}
	}
	tree := ast.Program{Symbols: []ast.Symbol{{Name: "v", Type: "int"}}, Cases: []ast.Case{{Name: "x"}}}
	if len(New(tree).contracts) != 1 {
		t.Fatal()
	}
	var program Program
	if json.Unmarshal([]byte(`{"@symbol":{"x":"bad"},"x":[]}`), &program) == nil {
		t.Fatal()
	}
}

func TestAdditionalRuntimeBranches(t *testing.T) {
	if v, ok := numeric(types.Int(2)); !ok || v != 2 {
		t.Fatal()
	}
	if v, ok := numeric(types.Double(3)); !ok || v != 3 {
		t.Fatal()
	}
	if _, ok := numeric(types.String("x")); ok {
		t.Fatal()
	}
	maxEnv, _ := cel.NewEnv(maxFunction())
	for _, src := range []string{"max(1.0, 2.0)", "max(3.0, 2)", "max(1, 2.0)"} {
		a, iss := maxEnv.Compile(src)
		if iss.Err() != nil {
			t.Fatal(iss.Err())
		}
		p, e := maxEnv.Program(a)
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = p.Eval(map[string]any{}); e != nil {
			t.Fatal(e)
		}
	}
	base := New(Symbol[int64]("v"), Case("x", Self("v"), With("v+1")))
	composed := New(base)
	if len(composed.AST.Cases) != 1 {
		t.Fatal()
	}
	var nilProgram *Program
	if json.Unmarshal([]byte(`{}`), nilProgram) == nil {
		t.Fatal()
	}
	if _, err := New().Compile(); err == nil {
		t.Fatal()
	}
	if _, err := New(Symbol[int64](""), Case("x")).Compile(); err == nil {
		t.Fatal()
	}
	r, _ := base.Compile()
	v := int64(0)
	b := Bind("v", Getter(func(context.Context) int64 { return v }), Setter(func(_ context.Context, x int64) { v = x }))
	var s Scope
	s.runtimeVersion = r.version
	s.continuations = map[string]continuation{"x": {RootCase: "bad", PC: 0, Version: r.version}}
	if err := r.Do(context.Background(), &s, "x", b); err == nil {
		t.Fatal()
	}
	s.continuations = map[string]continuation{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(r.Do(ctx, &s, "x", b), context.Canceled) {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", Bind("v", Getter(func(context.Context) int64 { return 1 }), Setter(func(context.Context, float64) {}))); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", Bind("v", b, b)); err == nil {
		t.Fatal()
	}

	ci := Symbol[func(int64, int64) (int64, error)]("f").(symbolDeclaration).contract
	call, _ := functionAdapter(ci, func(int64, int64) (int64, error) { return 0, errors.New("x") })
	if !types.IsError(call(context.Background(), []ref.Val{types.Int(1), types.Int(2)})) {
		t.Fatal()
	}
	cf := Symbol[func(float64, float64) (float64, error)]("f").(symbolDeclaration).contract
	call, _ = functionAdapter(cf, func(float64, float64) (float64, error) { return 0, errors.New("x") })
	if !types.IsError(call(context.Background(), []ref.Val{types.Double(1), types.Double(2)})) {
		t.Fatal()
	}
}
