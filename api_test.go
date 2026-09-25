package causal_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"causal"
	"causal/ast"
)

func TestNewAPIExecutionWaitAndFullBindingContract(t *testing.T) {
	now := time.Unix(10, 0)
	health, damage := float64(10), float64(3)
	p := causal.New(
		causal.Symbol[float64]("health"), causal.Symbol[float64]("damage"),
		causal.Case("attack", causal.Skip("damage <= 0"), causal.Self("health"), causal.With("max(0, health-damage)"), causal.Wait(`duration("1s")`), causal.With("max(0, health-damage)")),
	)
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var s causal.Scope
	s.SetClock(func() time.Time { return now })
	bindings := []causal.Binding{
		causal.Bind("health", causal.Getter(func() float64 { return health }), causal.Setter(func(v float64) { health = v })),
		causal.Bind("damage", causal.Getter(func() float64 { return damage })),
	}
	if err := r.Do(context.Background(), &s, "attack", bindings...); err != nil {
		t.Fatal(err)
	}
	if health != 7 {
		t.Fatalf("health=%v", health)
	}
	if err := r.Do(context.Background(), &s, "attack", bindings[0]); err == nil || !strings.Contains(err.Error(), "damage") {
		t.Fatalf("missing full contract: %v", err)
	}
	now = now.Add(time.Second)
	if err := r.Do(context.Background(), &s, "attack", bindings...); err != nil {
		t.Fatal(err)
	}
	if health != 4 {
		t.Fatalf("health=%v", health)
	}
}

func TestFunctionSymbolsAndErrors(t *testing.T) {
	v := int64(2)
	p := causal.New(causal.Symbol[int64]("v"), causal.Symbol[func(context.Context, int64) (int64, error)]("twice"), causal.Case("x", causal.Self("v"), causal.With("twice(v)")))
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var s causal.Scope
	value := causal.Bind("v", causal.Getter(func() int64 { return v }), causal.Setter(func(x int64) { v = x }))
	if err := r.Do(context.Background(), &s, "x", value, causal.Bind("twice", func(context.Context, int64) (int64, error) { return 0, errors.New("boom") })); err == nil {
		t.Fatal("expected function error")
	}
	if v != 2 {
		t.Fatal("failed segment committed")
	}
	if err := r.Do(context.Background(), &s, "x", value, causal.Bind("twice", func(_ context.Context, x int64) (int64, error) { return x * 2, nil })); err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Fatalf("v=%d", v)
	}
}

func TestASTJSONCanonical(t *testing.T) {
	p := causal.New(causal.Symbol[float64]("health"), causal.Case("attack", causal.Self("health"), causal.With("health-1.0")))
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"@symbol":{"health":"double"},"attack":[{"self":"health"},{"with":"health-1.0"}]}` {
		t.Fatalf("json=%s", b)
	}
	var tree ast.Program
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(tree)
	if string(again) != string(b) {
		t.Fatalf("roundtrip=%s", again)
	}
	var root causal.Program
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Compile(); err != nil {
		t.Fatalf("JSON symbols did not compile: %v", err)
	}
}

func TestCompleteProgramFromJSON(t *testing.T) {
	source := `{"@symbol":{"value":"string","choose":"(string,bool,uint)string"},"x":[{"self":"value"},{"with":"choose(\"ok\",true,2u)"}]}`
	var program causal.Program
	if err := json.Unmarshal([]byte(source), &program); err != nil {
		t.Fatal(err)
	}
	runtime, err := program.Compile()
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	var scope causal.Scope
	bindings := []causal.Binding{
		causal.Bind("value", causal.Getter(func() string { return value }), causal.Setter(func(v string) { value = v })),
		causal.Bind("choose", func(text string, enabled bool, count uint64) string {
			if enabled && count == 2 {
				return text
			}
			return ""
		}),
	}
	if err := runtime.Do(context.Background(), &scope, "x", bindings[0], causal.Bind("choose", func(string) string { return "" })); err == nil {
		t.Fatal("accepted incompatible JSON function binding")
	}
	if err := runtime.Do(context.Background(), &scope, "x", bindings...); err != nil {
		t.Fatal(err)
	}
	if value != "ok" {
		t.Fatalf("value=%q", value)
	}
}

func TestBindingValidationAndZeroScope(t *testing.T) {
	p := causal.New(causal.Symbol[int64]("v"), causal.Case("x", causal.Self("v"), causal.With("v+1")))
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var s causal.Scope
	get := causal.Getter(func() int64 { return 1 })
	for _, bindings := range [][]causal.Binding{{}, {causal.Bind("v", get)}, {causal.Bind("v", causal.Getter(func() float64 { return 1 }))}, {causal.Bind("other", get)}} {
		if err := r.Do(context.Background(), &s, "x", bindings...); err == nil {
			t.Fatalf("accepted %#v", bindings)
		}
	}
	if err := r.Do(context.Background(), nil, "x"); err == nil {
		t.Fatal("accepted nil scope")
	}
}

func TestCompileValidation(t *testing.T) {
	tests := []causal.Program{
		causal.New(causal.Case("x", causal.With("1"))),
		causal.New(causal.Symbol[int64]("v"), causal.Case("x", causal.Skip("v"))),
		causal.New(causal.Symbol[int64]("v"), causal.Case("x", causal.Wait("v"))),
		causal.New(causal.Symbol[int64]("v"), causal.Symbol[int64]("v"), causal.Case("x")),
		causal.New(causal.Symbol[int64]("v"), causal.Case("x"), causal.Case("x")),
	}
	for i, p := range tests {
		if _, err := p.Compile(); err == nil {
			t.Fatalf("case %d compiled", i)
		}
	}
}

func TestScopeJSONContinuation(t *testing.T) {
	now := time.Unix(1, 0)
	v := int64(0)
	p := causal.New(causal.Symbol[int64]("v"), causal.Case("x", causal.Self("v"), causal.With("v+1"), causal.Wait(`duration("1s")`), causal.With("v+1")))
	r, _ := p.Compile()
	b := causal.Bind("v", causal.Getter(func() int64 { return v }), causal.Setter(func(x int64) { v = x }))
	var s causal.Scope
	s.SetClock(func() time.Time { return now })
	if err := r.Do(context.Background(), &s, "x", b); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(&s)
	var restored causal.Scope
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	restored.SetClock(func() time.Time { return now.Add(time.Second) })
	if err := r.Do(context.Background(), &restored, "x", b); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("v=%d", v)
	}
}

func TestArbitraryFunctionSymbolSignature(t *testing.T) {
	value := ""
	p := causal.New(
		causal.Symbol[string]("value"),
		causal.Symbol[func(context.Context, string, bool, uint64) (string, error)]("choose"),
		causal.Case("x", causal.Self("value"), causal.With(`choose("ok", true, 2u)`)),
	)
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var scope causal.Scope
	state := causal.Bind("value", causal.Getter(func() string { return value }), causal.Setter(func(v string) { value = v }))
	if err := r.Do(context.Background(), &scope, "x", state); err == nil || !strings.Contains(err.Error(), "choose") {
		t.Fatalf("missing function was not prevalidated: %v", err)
	}
	fn := func(_ context.Context, text string, enabled bool, count uint64) (string, error) {
		if enabled && count == 2 {
			return text, nil
		}
		return "", errors.New("bad arguments")
	}
	if err := r.Do(context.Background(), &scope, "x", state, causal.Bind("choose", fn)); err != nil {
		t.Fatal(err)
	}
	if value != "ok" {
		t.Fatalf("value=%q", value)
	}
}
