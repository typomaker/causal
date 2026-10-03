package causal

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"causal/ast"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

func TestStaticKindsAndContracts(t *testing.T) {
	decls := []Declaration{Symbol[bool]("b"), Symbol[string]("s"), Symbol[int64]("i"), Symbol[uint64]("u"), Symbol[float64]("f"), Symbol[time.Duration]("d"), Symbol[time.Time]("t"), Symbol[struct{}]("bad")}
	for i, d := range decls[:7] {
		if d.(symbolDeclaration).contract.err != nil {
			t.Fatal(i)
		}
	}
	if decls[7].(symbolDeclaration).contract.err == nil {
		t.Fatal("unsupported type")
	}
	if kind, ok := kindForType(reflect.TypeFor[bool]()); !ok || kind != "bool" {
		t.Fatal("kinds")
	}
}

func TestAllFunctionAdapters(t *testing.T) {
	ctx := context.Background()
	i := types.Int(2)
	f := types.Double(2)
	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	ts := types.Timestamp{Time: now}
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
		{Symbol[func(time.Time) time.Time]("x"), func(value time.Time) time.Time { return value.Add(time.Hour) }, []ref.Val{ts}},
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

func TestTypedFunctionBindings(t *testing.T) {
	ctx := context.Background()
	i := types.Int(1)
	tests := []struct {
		binding Binding
		args    []ref.Val
	}{
		{BindFunc0("f", func() int64 { return 1 }), nil},
		{BindFunc0Err("f", func() (int64, error) { return 1, nil }), nil},
		{BindContextFunc0("f", func(context.Context) int64 { return 1 }), nil},
		{BindContextFunc0Err("f", func(context.Context) (int64, error) { return 1, nil }), nil},
		{BindFunc1("f", func(a int64) int64 { return a }), []ref.Val{i}},
		{BindFunc1Err("f", func(a int64) (int64, error) { return a, nil }), []ref.Val{i}},
		{BindContextFunc1("f", func(_ context.Context, a int64) int64 { return a }), []ref.Val{i}},
		{BindContextFunc1Err("f", func(_ context.Context, a int64) (int64, error) { return a, nil }), []ref.Val{i}},
		{BindFunc2("f", func(a, b int64) int64 { return a + b }), []ref.Val{i, i}},
		{BindFunc2Err("f", func(a, b int64) (int64, error) { return a + b, nil }), []ref.Val{i, i}},
		{BindContextFunc2("f", func(_ context.Context, a, b int64) int64 { return a + b }), []ref.Val{i, i}},
		{BindContextFunc2Err("f", func(_ context.Context, a, b int64) (int64, error) { return a + b, nil }), []ref.Val{i, i}},
		{BindFunc3("f", func(a, b, c int64) int64 { return a + b + c }), []ref.Val{i, i, i}},
		{BindFunc3Err("f", func(a, b, c int64) (int64, error) { return a + b + c, nil }), []ref.Val{i, i, i}},
		{BindContextFunc3("f", func(_ context.Context, a, b, c int64) int64 { return a + b + c }), []ref.Val{i, i, i}},
		{BindContextFunc3Err("f", func(_ context.Context, a, b, c int64) (int64, error) { return a + b + c, nil }), []ref.Val{i, i, i}},
		{BindFunc4("f", func(a, b, c, d int64) int64 { return a + b + c + d }), []ref.Val{i, i, i, i}},
		{BindFunc4Err("f", func(a, b, c, d int64) (int64, error) { return a + b + c + d, nil }), []ref.Val{i, i, i, i}},
		{BindContextFunc4("f", func(_ context.Context, a, b, c, d int64) int64 { return a + b + c + d }), []ref.Val{i, i, i, i}},
		{BindContextFunc4Err("f", func(_ context.Context, a, b, c, d int64) (int64, error) { return a + b + c + d, nil }), []ref.Val{i, i, i, i}},
		{BindFunc5("f", func(a, b, c, d, e int64) int64 { return a + b + c + d + e }), []ref.Val{i, i, i, i, i}},
		{BindFunc5Err("f", func(a, b, c, d, e int64) (int64, error) { return a + b + c + d + e, nil }), []ref.Val{i, i, i, i, i}},
		{BindContextFunc5("f", func(_ context.Context, a, b, c, d, e int64) int64 { return a + b + c + d + e }), []ref.Val{i, i, i, i, i}},
		{BindContextFunc5Err("f", func(_ context.Context, a, b, c, d, e int64) (int64, error) { return a + b + c + d + e, nil }), []ref.Val{i, i, i, i, i}},
		{BindFunc6("f", func(a, b, c, d, e, f int64) int64 { return a + b + c + d + e + f }), []ref.Val{i, i, i, i, i, i}},
		{BindFunc6Err("f", func(a, b, c, d, e, f int64) (int64, error) { return a + b + c + d + e + f, nil }), []ref.Val{i, i, i, i, i, i}},
		{BindContextFunc6("f", func(_ context.Context, a, b, c, d, e, f int64) int64 { return a + b + c + d + e + f }), []ref.Val{i, i, i, i, i, i}},
		{BindContextFunc6Err("f", func(_ context.Context, a, b, c, d, e, f int64) (int64, error) { return a + b + c + d + e + f, nil }), []ref.Val{i, i, i, i, i, i}},
		{BindFunc7("f", func(a, b, c, d, e, f, g int64) int64 { return a + b + c + d + e + f + g }), []ref.Val{i, i, i, i, i, i, i}},
		{BindFunc7Err("f", func(a, b, c, d, e, f, g int64) (int64, error) { return a + b + c + d + e + f + g, nil }), []ref.Val{i, i, i, i, i, i, i}},
		{BindContextFunc7("f", func(_ context.Context, a, b, c, d, e, f, g int64) int64 { return a + b + c + d + e + f + g }), []ref.Val{i, i, i, i, i, i, i}},
		{BindContextFunc7Err("f", func(_ context.Context, a, b, c, d, e, f, g int64) (int64, error) {
			return a + b + c + d + e + f + g, nil
		}), []ref.Val{i, i, i, i, i, i, i}},
		{BindFunc8("f", func(a, b, c, d, e, f, g, h int64) int64 { return a + b + c + d + e + f + g + h }), []ref.Val{i, i, i, i, i, i, i, i}},
		{BindFunc8Err("f", func(a, b, c, d, e, f, g, h int64) (int64, error) { return a + b + c + d + e + f + g + h, nil }), []ref.Val{i, i, i, i, i, i, i, i}},
		{BindContextFunc8("f", func(_ context.Context, a, b, c, d, e, f, g, h int64) int64 { return a + b + c + d + e + f + g + h }), []ref.Val{i, i, i, i, i, i, i, i}},
		{BindContextFunc8Err("f", func(_ context.Context, a, b, c, d, e, f, g, h int64) (int64, error) {
			return a + b + c + d + e + f + g + h, nil
		}), []ref.Val{i, i, i, i, i, i, i, i}},
	}
	for index, test := range tests {
		if test.binding.err != nil || test.binding.function == nil {
			t.Fatalf("binding %d: %v", index, test.binding.err)
		}
		if result := callBoundFunction(ctx, test.binding.function, test.args); types.IsError(result) {
			t.Fatalf("binding %d: %v", index, result)
		}
	}
	for _, index := range []int{16, 20, 24, 28, 32} {
		test := tests[index]
		for argument := range test.args {
			invalid := append([]ref.Val(nil), test.args...)
			invalid[argument] = types.String("bad")
			if !types.IsError(callBoundFunction(ctx, test.binding.function, invalid)) {
				t.Fatalf("binding %d accepted invalid argument %d", index, argument)
			}
		}
	}

	failing := BindFunc1Err("f", func(int64) (int64, error) { return 0, errors.New("failed") })
	if !types.IsError(callBoundFunction(ctx, failing.function, []ref.Val{i})) {
		t.Fatal("typed binding lost function error")
	}
	if !types.IsError(callBoundFunction(ctx, failing.function, nil)) {
		t.Fatal("typed binding accepted wrong argument count")
	}
	if !types.IsError(callBoundFunction(ctx, failing.function, []ref.Val{types.String("bad")})) {
		t.Fatal("typed binding accepted wrong argument type")
	}
	var nilFunction func(int64) int64
	if binding := BindFunc1("f", nilFunction); binding.err == nil {
		t.Fatal("typed binding accepted nil function")
	}
	if binding := newTypedBinding("f", nil, &boundFunction{}); binding.err == nil {
		t.Fatal("typed binding accepted untyped nil")
	}
	if binding := newTypedBinding("f", func(int) int { return 0 }, &boundFunction{}); binding.err == nil {
		t.Fatal("typed binding accepted unsupported signature")
	}
	for _, result := range []ref.Val{
		typedResult(true, nil), typedResult("x", nil), typedResult(int64(1), nil),
		typedResult(uint64(1), nil), typedResult(float64(1), nil),
		typedResult(time.Second, nil), typedResult(time.Unix(1, 0), nil), typedResult([]int64{1}, nil),
	} {
		if types.IsError(result) {
			t.Fatal(result)
		}
	}
}

func TestTypedRuntimeArities(t *testing.T) {
	value := int64(0)
	runtime, err := New(
		Symbol[int64]("value"),
		Symbol[func() int64]("f0"),
		Symbol[func(int64) int64]("f1"),
		Symbol[func(int64, int64) int64]("f2"),
		Symbol[func(int64, int64, int64) int64]("f3"),
		Symbol[func(int64, int64, int64, int64) int64]("f4"),
		Symbol[func(int64, int64, int64, int64, int64) int64]("f5"),
		Symbol[func(int64, int64, int64, int64, int64, int64) int64]("f6"),
		Symbol[func(int64, int64, int64, int64, int64, int64, int64) int64]("f7"),
		Symbol[func(int64, int64, int64, int64, int64, int64, int64, int64) int64]("f8"),
		Case("run", Self("value"), With("f0()+f1(1)+f2(1,1)+f3(1,1,1)+f4(1,1,1,1)+f5(1,1,1,1,1)+f6(1,1,1,1,1,1)+f7(1,1,1,1,1,1,1)+f8(1,1,1,1,1,1,1,1)")),
	).Compile()
	if err != nil {
		t.Fatal(err)
	}
	bindings := []Binding{
		Bind("value", &value),
		BindFunc0("f0", func() int64 { return 0 }),
		BindFunc1("f1", func(a int64) int64 { return a }),
		BindFunc2("f2", func(a, b int64) int64 { return a + b }),
		BindFunc3("f3", func(a, b, c int64) int64 { return a + b + c }),
		BindFunc4("f4", func(a, b, c, d int64) int64 { return a + b + c + d }),
		BindFunc5("f5", func(a, b, c, d, e int64) int64 { return a + b + c + d + e }),
		BindFunc6("f6", func(a, b, c, d, e, f int64) int64 { return a + b + c + d + e + f }),
		BindFunc7("f7", func(a, b, c, d, e, f, g int64) int64 { return a + b + c + d + e + f + g }),
		BindFunc8("f8", func(a, b, c, d, e, f, g, h int64) int64 { return a + b + c + d + e + f + g + h }),
	}
	var scope Scope
	if err := runtime.Do(context.Background(), &scope, "run", bindings...); err != nil {
		t.Fatal(err)
	}
	if value != 36 {
		t.Fatalf("value=%d", value)
	}
}

func TestSegmentPoolCleanupAndOrderCapacity(t *testing.T) {
	runtime, err := New(
		Symbol[int64]("a"), Symbol[int64]("b"),
		Case("run", Self("a"), With("a+1"), Self("b"), With("b+1")),
	).Compile()
	if err != nil {
		t.Fatal(err)
	}
	scratch := runtime.segmentPool.Get().(*segment)
	if cap(scratch.order) < 2 {
		t.Fatalf("order capacity=%d", cap(scratch.order))
	}
	scratch.values[0] = segmentValue{snapshot: &struct{}{}, pending: types.Int(1), native: &struct{}{}, hasSnapshot: true, hasPending: true}
	scratch.order = append(scratch.order, 0)
	scratch.activation.context = context.Background()
	scratch.activation.bindings = []preparedBinding{{set: true}}
	runtime.releaseSegment(scratch)
	reused := runtime.segmentPool.Get().(*segment)
	defer runtime.releaseSegment(reused)
	if len(reused.order) != 0 || reused.values[0].hasSnapshot || reused.values[0].hasPending || reused.values[0].snapshot != nil || reused.values[0].pending != nil || reused.values[0].native != nil {
		t.Fatalf("pooled segment retained state: %#v", reused)
	}
	if reused.activation.context != nil || reused.activation.bindings != nil || reused.activation.segment != nil {
		t.Fatalf("pooled activation retained state: %#v", reused.activation)
	}
}

func TestValidateBindingsUsesCaseLayout(t *testing.T) {
	runtime, err := New(
		Symbol[int64]("first"), Symbol[int64]("second"), Symbol[int64]("unused"),
		Case("first_case", Skip("first > 0")),
		Case("second_case", Skip("second > 0")),
	).Compile()
	if err != nil {
		t.Fatal(err)
	}
	first, second, unused := int64(1), int64(2), int64(3)
	bindings, err := runtime.validateBindings(runtime.cases["first_case"], []Binding{
		Bind("first", &first), Bind("second", &second), Bind("unused", &unused),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("binding layout length=%d, want 1", len(bindings))
	}
	if bindings[0].name != "first" {
		t.Fatalf("binding layout contains %q, want first", bindings[0].name)
	}
}

func TestCaseBindingLayoutMapsFunctionIndexes(t *testing.T) {
	runtime, err := New(
		Symbol[int64]("before"), Symbol[func(int64) int64]("increment"), Symbol[int64]("value"),
		Case("run", Self("value"), With("increment(value)")),
	).Compile()
	if err != nil {
		t.Fatal(err)
	}
	value := int64(1)
	if err := runtime.Do(context.Background(), &Scope{}, "run",
		Bind("value", &value), BindFunc1("increment", func(v int64) int64 { return v + 1 }),
	); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("value=%d, want 2", value)
	}
}

func callBoundFunction(ctx context.Context, function *boundFunction, args []ref.Val) ref.Val {
	switch function.typedArity {
	case 0:
		return function.call0(ctx)
	case 1:
		if len(args) != 1 {
			return types.NewErr("wrong argument count")
		}
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
		return types.NewErr("unsupported test arity")
	}
}

func TestNestedCasesReadinessAndFailures(t *testing.T) {
	p := New(Symbol[int64]("v"), Case("root", Case("child", Self("v"), Skip("true"), With("v+100")), With("v+1")), Case("writer", Self("v"), With("v+1")))
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	v := int64(1)
	b := Bind("v", &v)
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

func TestCompiledExpressionMetadata(t *testing.T) {
	program := New(
		Symbol[int64]("health"), Symbol[int64]("base"), Symbol[int64]("armor"),
		Symbol[func(int64, int64) int64]("damage"),
		Case("attack", Self("health"), With("health - damage(base, armor)")),
	)
	runtime, err := program.Compile()
	if err != nil {
		t.Fatal(err)
	}
	expr := runtime.cases["attack"].code[0].expr
	if !reflect.DeepEqual(expr.reads, []string{"armor", "base", "health"}) {
		t.Fatalf("reads=%v", expr.reads)
	}
	if !reflect.DeepEqual(expr.functions, []string{"damage"}) {
		t.Fatalf("functions=%v", expr.functions)
	}
	if expr.program == nil {
		t.Fatal("CEL program was not built during Compile")
	}

	env, err := cel.NewEnv(
		cel.Variable("base", cel.IntType),
		cel.Variable("xs", cel.ListType(cel.IntType)),
		cel.Variable("m", cel.MapType(cel.StringType, cel.IntType)),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{`xs.map(x, x + base)`, `m.key + base`, `[base, 1][0]`, `{"x": base}.x`} {
		requirements := map[string]struct{}{}
		compiled, compileErr := compileExpr(env, source, requirements)
		if compileErr != nil {
			t.Fatalf("compile %q: %v", source, compileErr)
		}
		if compiled.program == nil || len(compiled.reads) == 0 {
			t.Fatalf("metadata for %q: %#v", source, compiled)
		}
	}
	if _, err := compileExpr(env, " ", map[string]struct{}{}); err == nil {
		t.Fatal("empty expression compiled")
	}
	if _, err := compileExpr(env, "missing", map[string]struct{}{}); err == nil {
		t.Fatal("invalid expression compiled")
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
	p := New(Symbol[int64]("v"), Case("x", Self("v"), With("v+1")))
	r, _ := p.Compile()
	v := int64(1)
	b := Bind("v", &v)
	var s Scope
	if err := r.Do(context.Background(), &s, "missing", b); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", b, b); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", Bind("", &v)); err == nil {
		t.Fatal()
	}
	bad := Bind("v", float64(1))
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
	for _, kind := range []string{"bool", "string", "int", "uint", "double", "duration", "timestamp"} {
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
	b := Bind("v", &v)
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
	wrong := float64(1)
	if err := r.Do(context.Background(), &s, "x", Bind("v", &wrong)); err == nil {
		t.Fatal()
	}
	if err := r.Do(context.Background(), &s, "x", Bind("v", func() int64 { return 1 })); err == nil {
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
