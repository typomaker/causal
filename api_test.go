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

var _ = map[causal.Scope]struct{}{}

func TestScopeComparableSnapshots(t *testing.T) {
	now := time.Unix(10, 0)
	var scope causal.Scope
	unchanged := scope
	_ = scope.Clock()
	if _, pending := scope.Pending("run"); pending {
		t.Fatal("zero scope has a pending continuation")
	}
	if scope != unchanged {
		t.Fatal("reading a scope changed its revision")
	}

	scope.SetClock(func() time.Time { return now })
	if scope == unchanged {
		t.Fatal("SetClock did not change the scope revision")
	}

	value := int64(0)
	runtime, err := causal.New(
		causal.Symbol[int64]("value"),
		causal.Case("run", causal.Self("value"), causal.With("value + 1"), causal.Wait(`duration("1s")`)),
	).Compile()
	if err != nil {
		t.Fatal(err)
	}

	beforeRun := scope
	if err := runtime.Do(context.Background(), &scope, "run", causal.Bind("value", &value)); err != nil {
		t.Fatal(err)
	}
	if scope == beforeRun {
		t.Fatal("execution did not change the scope revision")
	}
	if _, pending := beforeRun.Pending("run"); pending {
		t.Fatal("an older scope snapshot observed a new continuation")
	}
	if _, pending := scope.Pending("run"); !pending {
		t.Fatal("new scope snapshot has no continuation")
	}
}

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
		causal.Bind("health", &health),
		causal.Bind("damage", &damage),
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
	value := causal.Bind("v", &v)
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
		causal.Bind("value", &value),
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

func TestMatchingJSONAndGoSymbolsAreMerged(t *testing.T) {
	var fromJSON causal.Program
	source := `{"@symbol":{"increment":"(int)int","value":"int"},"x":[{"self":"value"},{"with":"increment(value)"}]}`
	if err := json.Unmarshal([]byte(source), &fromJSON); err != nil {
		t.Fatal(err)
	}

	program := causal.New(
		fromJSON,
		causal.Symbol[int64]("value"),
		causal.Symbol[func(context.Context, int64) (int64, error)]("increment"),
	)
	runtime, err := program.Compile()
	if err != nil {
		t.Fatal(err)
	}

	value := int64(1)
	state := causal.Bind("value", &value)
	var scope causal.Scope
	if err := runtime.Do(context.Background(), &scope, "x", state, causal.Bind("increment", func(int64) int64 { return 0 })); err == nil {
		t.Fatal("merged function symbol lost its Go signature")
	}
	if err := runtime.Do(context.Background(), &scope, "x", state, causal.Bind("increment", func(_ context.Context, current int64) (int64, error) {
		return current + 1, nil
	})); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("value=%d", value)
	}
}

func TestConflictingJSONAndGoSymbolsAreRejected(t *testing.T) {
	var fromJSON causal.Program
	if err := json.Unmarshal([]byte(`{"@symbol":{"value":"int"},"x":[]}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	_, err := causal.New(fromJSON, causal.Symbol[float64]("value")).Compile()
	if err == nil || !strings.Contains(err.Error(), "conflicting declarations") {
		t.Fatalf("error=%v", err)
	}
}

func TestBindingValidationAndZeroScope(t *testing.T) {
	p := causal.New(causal.Symbol[int64]("v"), causal.Case("x", causal.Self("v"), causal.With("v+1")))
	r, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var s causal.Scope
	good, wrong := int64(1), float64(1)
	for _, bindings := range [][]causal.Binding{{}, {causal.Bind("v", &wrong)}, {causal.Bind("v", good)}, {causal.Bind("other", &good)}} {
		if err := r.Do(context.Background(), &s, "x", bindings...); err == nil {
			t.Fatalf("accepted %#v", bindings)
		}
	}
	if err := r.Do(context.Background(), nil, "x"); err == nil {
		t.Fatal("accepted nil scope")
	}
}

func TestPointerBindingValidation(t *testing.T) {
	program := causal.New(
		causal.Symbol[int64]("value"),
		causal.Symbol[func(int64) int64]("transform"),
		causal.Case("run", causal.Self("value"), causal.With("transform(value)")),
	)
	runtime, err := program.Compile()
	if err != nil {
		t.Fatal(err)
	}

	value := int64(1)
	wrong := float64(1)
	var nilValue *int64
	validFunction := func(v int64) int64 { return v + 1 }
	tests := []struct {
		name     string
		bindings []causal.Binding
	}{
		{"nil pointer", []causal.Binding{causal.Bind("value", nilValue), causal.Bind("transform", validFunction)}},
		{"wrong pointer type", []causal.Binding{causal.Bind("value", &wrong), causal.Bind("transform", validFunction)}},
		{"non-pointer value", []causal.Binding{causal.Bind("value", value), causal.Bind("transform", validFunction)}},
		{"pointer for function", []causal.Binding{causal.Bind("value", &value), causal.Bind("transform", &validFunction)}},
		{"function for value", []causal.Binding{causal.Bind("value", func() int64 { return value }), causal.Bind("transform", validFunction)}},
		{"missing binding", []causal.Binding{causal.Bind("value", &value)}},
		{"duplicate binding", []causal.Binding{causal.Bind("value", &value), causal.Bind("value", &value), causal.Bind("transform", validFunction)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var scope causal.Scope
			if err := runtime.Do(context.Background(), &scope, "run", test.bindings...); err == nil {
				t.Fatal("binding validation succeeded")
			}
			if value != 1 {
				t.Fatalf("validation changed value to %d", value)
			}
		})
	}
}

func TestPointerBindingAtomicityAndRepeatedWrite(t *testing.T) {
	t.Run("failed segment does not write any pointer", func(t *testing.T) {
		x, y := int64(10), int64(20)
		program := causal.New(
			causal.Symbol[int64]("x"),
			causal.Symbol[int64]("y"),
			causal.Symbol[func(int64) (int64, error)]("fail"),
			causal.Case("run", causal.Self("x"), causal.With("x+1"), causal.Self("y"), causal.With("fail(y)")),
		)
		runtime, err := program.Compile()
		if err != nil {
			t.Fatal(err)
		}
		var scope causal.Scope
		err = runtime.Do(context.Background(), &scope, "run",
			causal.Bind("x", &x),
			causal.Bind("y", &y),
			causal.Bind("fail", func(int64) (int64, error) { return 0, errors.New("failed") }),
		)
		if err == nil {
			t.Fatal("expected expression failure")
		}
		if x != 10 || y != 20 {
			t.Fatalf("partial commit: x=%d y=%d", x, y)
		}
	})

	t.Run("later expression reads pending value", func(t *testing.T) {
		x := int64(10)
		program := causal.New(
			causal.Symbol[int64]("x"),
			causal.Case("run", causal.Self("x"), causal.With("x+1"), causal.Self("x"), causal.With("x*2")),
		)
		runtime, err := program.Compile()
		if err != nil {
			t.Fatal(err)
		}
		var scope causal.Scope
		if err := runtime.Do(context.Background(), &scope, "run", causal.Bind("x", &x)); err != nil {
			t.Fatal(err)
		}
		if x != 22 {
			t.Fatalf("x=%d, want 22", x)
		}
	})
}

func TestPointerStagingSegments(t *testing.T) {
	t.Run("snapshot is reused and pending wins", func(t *testing.T) {
		value := int64(10)
		program := causal.New(
			causal.Symbol[int64]("value"),
			causal.Symbol[func() int64]("mutate"),
			causal.Case("run", causal.Self("value"), causal.With("value + value + mutate()"), causal.With("value + 1")),
		)
		runtime, err := program.Compile()
		if err != nil {
			t.Fatal(err)
		}
		var scope causal.Scope
		err = runtime.Do(context.Background(), &scope, "run",
			causal.Bind("value", &value),
			causal.Bind("mutate", func() int64 { value = 100; return 1 }),
		)
		if err != nil {
			t.Fatal(err)
		}
		if value != 22 {
			t.Fatalf("value=%d, want 22", value)
		}
	})

	t.Run("Wait starts a fresh snapshot on resume", func(t *testing.T) {
		now := time.Unix(100, 0)
		value := int64(1)
		program := causal.New(causal.Symbol[int64]("value"), causal.Case("run",
			causal.Self("value"), causal.With("value + 1"), causal.Wait(`duration("1s")`), causal.With("value + 1"),
		))
		runtime, err := program.Compile()
		if err != nil {
			t.Fatal(err)
		}
		var scope causal.Scope
		scope.SetClock(func() time.Time { return now })
		binding := causal.Bind("value", &value)
		if err := runtime.Do(context.Background(), &scope, "run", binding); err != nil {
			t.Fatal(err)
		}
		if value != 2 {
			t.Fatalf("committed value=%d", value)
		}
		value = 40
		now = now.Add(time.Second)
		if err := runtime.Do(context.Background(), &scope, "run", binding); err != nil {
			t.Fatal(err)
		}
		if value != 41 {
			t.Fatalf("resumed value=%d, want 41", value)
		}
	})

	t.Run("validation does not invoke functions", func(t *testing.T) {
		calls := 0
		program := causal.New(
			causal.Symbol[int64]("value"), causal.Symbol[func(int64) int64]("change"),
			causal.Case("run", causal.Self("value"), causal.With("change(value)")),
		)
		runtime, err := program.Compile()
		if err != nil {
			t.Fatal(err)
		}
		var scope causal.Scope
		err = runtime.Do(context.Background(), &scope, "run", causal.Bind("change", func(v int64) int64 { calls++; return v }))
		if err == nil {
			t.Fatal("missing value binding accepted")
		}
		if calls != 0 {
			t.Fatalf("function called %d times during validation", calls)
		}
	})
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
	b := causal.Bind("v", &v)
	var s causal.Scope
	if _, pending := s.Pending("x"); pending {
		t.Fatal("zero scope has a pending continuation")
	}
	s.SetClock(func() time.Time { return now })
	if err := r.Do(context.Background(), &s, "x", b); err != nil {
		t.Fatal(err)
	}
	deadline, pending := s.Pending("x")
	if !pending || !deadline.Equal(now.Add(time.Second)) {
		t.Fatalf("pending deadline=%s exists=%t", deadline, pending)
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
	if _, pending := restored.Pending("x"); pending {
		t.Fatal("completed scope remains pending")
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
	state := causal.Bind("value", &value)
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
