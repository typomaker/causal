//go:build legacy

package causal_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"causal"
)

func TestStagingPendingAndLocalSkip(t *testing.T) {
	ctx := context.Background()
	health, armor := int64(10), int64(5)
	schema := causal.Schema(
		causal.Case("root",
			causal.Self("health"), causal.With("health - 3"),
			causal.Case("child", causal.Skip("true"), causal.Self("health"), causal.With("0")),
			causal.Self("health"), causal.With("health + armor"),
		),
	)
	e, err := causal.Compile(schema)
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(
		causal.State("health", causal.Getter(func(context.Context) int64 { return health }), causal.Setter(func(_ context.Context, v int64) { health = v })),
		causal.State("armor", causal.Getter(func(context.Context) int64 { return armor })),
	)
	if err := e.Do(ctx, s, "root"); err != nil {
		t.Fatal(err)
	}
	if health != 12 {
		t.Fatalf("health = %d, want 12", health)
	}
}

func TestSelfFlowsThroughNestedCase(t *testing.T) {
	value := int64(1)
	e, err := causal.Compile(causal.Schema(causal.Case("root",
		causal.Case("select", causal.Self("value")),
		causal.With("value + 1"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(causal.State("value",
		causal.Getter(func(context.Context) int64 { return value }),
		causal.Setter(func(_ context.Context, v int64) { value = v }),
	))
	if err := e.Do(context.Background(), s, "root"); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("value = %d, want 2", value)
	}
	if err := e.Do(context.Background(), s, "select"); err == nil {
		t.Fatal("nested Case must not be directly executable")
	}
}

func TestWaitCommitsAndResumes(t *testing.T) {
	now := time.Unix(100, 0)
	value := int64(0)
	e, err := causal.Compile(causal.Schema(causal.Case("process",
		causal.Self("value"), causal.With("value + 1"),
		causal.Wait(`duration("3s")`),
		causal.Self("value"), causal.With("value + 10"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(causal.State("value", causal.Getter(func(context.Context) int64 { return value }), causal.Setter(func(_ context.Context, v int64) { value = v })))
	s.SetClock(func() time.Time { return now })
	if err := e.Do(context.Background(), s, "process"); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("first segment value = %d", value)
	}
	if err := e.Do(context.Background(), s, "process"); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("early resume changed value to %d", value)
	}
	now = now.Add(3 * time.Second)
	if err := e.Do(context.Background(), s, "process"); err != nil {
		t.Fatal(err)
	}
	if value != 11 {
		t.Fatalf("resumed value = %d", value)
	}
}

func TestFunctionErrorDoesNotCommit(t *testing.T) {
	v := int64(7)
	e, err := causal.Compile(causal.Schema(
		causal.Func("fail", func(context.Context, int64) (int64, error) { return 0, errors.New("boom") }),
		causal.Case("x", causal.Self("v"), causal.With("v + 1"), causal.With("fail(v)")),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(causal.State("v", causal.Getter(func(context.Context) int64 { return v }), causal.Setter(func(_ context.Context, x int64) { v = x })))
	if err := e.Do(context.Background(), s, "x"); err == nil {
		t.Fatal("expected error")
	}
	if v != 7 {
		t.Fatalf("failed segment committed value %d", v)
	}
}

func TestDependencyMarksReadyButDoesNotRun(t *testing.T) {
	a, b := int64(1), int64(0)
	e, err := causal.Compile(causal.Schema(
		causal.Case("A", causal.Self("a"), causal.With("a + 1")),
		causal.Case("B", causal.Self("b"), causal.With("a * 10")),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(
		causal.State("a", causal.Getter(func(context.Context) int64 { return a }), causal.Setter(func(_ context.Context, v int64) { a = v })),
		causal.State("b", causal.Getter(func(context.Context) int64 { return b }), causal.Setter(func(_ context.Context, v int64) { b = v })),
	)
	if err := e.Do(context.Background(), s, "A"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	if !jsonReady(data, "B") {
		t.Fatal("B was not marked ready")
	}
	if b != 0 {
		t.Fatalf("B ran implicitly: b=%d", b)
	}
}

func TestDeterministicSetterOrderAndContextCancellation(t *testing.T) {
	var order []string
	a, b := int64(1), int64(2)
	e, _ := causal.Compile(causal.Schema(causal.Case("x", causal.Self("b"), causal.With("b+1"), causal.Self("a"), causal.With("a+1"), causal.Self("b"), causal.With("b+1"))))
	s := causal.Scope(
		causal.State("a", causal.Getter(func(context.Context) int64 { return a }), causal.Setter(func(_ context.Context, v int64) { order = append(order, "a"); a = v })),
		causal.State("b", causal.Getter(func(context.Context) int64 { return b }), causal.Setter(func(_ context.Context, v int64) { order = append(order, "b"); b = v })),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Do(ctx, s, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(order) != 0 {
		t.Fatal("cancelled segment committed")
	}
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("setter order = %v", order)
	}
}

func TestContinuationJSONAndVersionMismatch(t *testing.T) {
	now := time.Unix(10, 0)
	clock := causal.Clock(func() time.Time { return now })
	v := int64(0)
	e, _ := causal.Compile(causal.Schema(causal.Case("x", causal.Self("v"), causal.With("v+1"), causal.Wait(`duration("1s")`), causal.With("v+1"))))
	binding := causal.State("v", causal.Getter(func(context.Context) int64 { return v }), causal.Setter(func(_ context.Context, x int64) { v = x }))
	s := causal.Scope(binding)
	s.SetClock(clock)
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	restored := causal.Scope(binding)
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	restored.SetClock(clock)
	now = now.Add(time.Second)
	if err := e.Do(context.Background(), restored, "x"); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("restored continuation produced %d", v)
	}
	changed, _ := causal.Compile(causal.Schema(causal.Case("x", causal.Skip("false"))))
	broken := causal.Scope(binding)
	if err := json.Unmarshal(data, broken); err != nil {
		t.Fatal(err)
	}
	if err := changed.Do(context.Background(), broken, "x"); err == nil {
		t.Fatal("expected version mismatch")
	}
}

func TestSelfTargetIsDependency(t *testing.T) {
	v := int64(0)
	e, _ := causal.Compile(causal.Schema(causal.Case("writer", causal.Self("v"), causal.With("1")), causal.Case("constant", causal.Self("v"), causal.With("2"))))
	s := causal.Scope(causal.State("v", causal.Getter(func(context.Context) int64 { return v }), causal.Setter(func(_ context.Context, x int64) { v = x })))
	if err := e.Do(context.Background(), s, "writer"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	if !jsonReady(data, "constant") {
		t.Fatal("Self target dependency was not marked ready")
	}
}

func TestWaitInsideNestedCaseResumesInsideParent(t *testing.T) {
	now := time.Unix(100, 0)
	value := int64(0)
	e, err := causal.Compile(causal.Schema(causal.Case("root",
		causal.Self("value"),
		causal.Case("child",
			causal.With("value + 1"),
			causal.Wait(`duration("2s")`),
			causal.With("value + 10"),
		),
		causal.With("value + 100"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(causal.State("value",
		causal.Getter(func(context.Context) int64 { return value }),
		causal.Setter(func(_ context.Context, v int64) { value = v }),
	))
	s.SetClock(func() time.Time { return now })
	if err := e.Do(context.Background(), s, "root"); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("first segment = %d, want 1", value)
	}
	now = now.Add(2 * time.Second)
	if err := e.Do(context.Background(), s, "root"); err != nil {
		t.Fatal(err)
	}
	if value != 111 {
		t.Fatalf("resumed nested program = %d, want 111", value)
	}
}

func TestReadinessGraphUpdatesOneEdgeAtATime(t *testing.T) {
	x, y, z := int64(0), int64(0), int64(0)
	e, err := causal.Compile(causal.Schema(
		causal.Case("A", causal.Self("x"), causal.With("x + 1")),
		causal.Case("B", causal.Self("y"), causal.With("x + 1")),
		causal.Case("C", causal.Self("z"), causal.With("y + 1")),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(
		causal.State("x", causal.Getter(func(context.Context) int64 { return x }), causal.Setter(func(_ context.Context, v int64) { x = v })),
		causal.State("y", causal.Getter(func(context.Context) int64 { return y }), causal.Setter(func(_ context.Context, v int64) { y = v })),
		causal.State("z", causal.Getter(func(context.Context) int64 { return z }), causal.Setter(func(_ context.Context, v int64) { z = v })),
	)
	if err := e.Do(context.Background(), s, "A"); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s)
	if !jsonReady(state, "B") {
		t.Fatal("B must become ready after x changes")
	}
	if jsonReady(state, "C") {
		t.Fatal("C must not become ready transitively")
	}
	if y != 0 || z != 0 {
		t.Fatalf("dependent cases ran implicitly: y=%d z=%d", y, z)
	}

	if err := e.Do(context.Background(), s, "B"); err != nil {
		t.Fatal(err)
	}
	state, _ = json.Marshal(s)
	if !jsonReady(state, "C") {
		t.Fatal("C must become ready after B changes y")
	}
	if z != 0 {
		t.Fatalf("C ran implicitly: z=%d", z)
	}
}

func TestNestedCaseDependenciesBelongToRoot(t *testing.T) {
	x, out := int64(1), int64(0)
	e, err := causal.Compile(causal.Schema(
		causal.Case("writer", causal.Self("x"), causal.With("x + 1")),
		causal.Case("root", causal.Case("child", causal.Self("out"), causal.With("x"))),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := causal.Scope(
		causal.State("x", causal.Getter(func(context.Context) int64 { return x }), causal.Setter(func(_ context.Context, v int64) { x = v })),
		causal.State("out", causal.Getter(func(context.Context) int64 { return out }), causal.Setter(func(_ context.Context, v int64) { out = v })),
	)
	if err := e.Do(context.Background(), s, "writer"); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s)
	if !jsonReady(state, "root") {
		t.Fatal("root did not inherit child dependency on x")
	}
	if jsonReady(state, "child") {
		t.Fatal("nested child must not have independent readiness")
	}
}

func jsonReady(data []byte, name string) bool {
	var state struct {
		Readiness map[string]bool `json:"readiness"`
	}
	_ = json.Unmarshal(data, &state)
	return state.Readiness[name]
}
