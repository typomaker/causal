//go:build legacy

package causal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func intState(name string, value *int64, gets, sets *int) Binding {
	return State(name,
		Getter(func(context.Context) int64 {
			if gets != nil {
				*gets++
			}
			return *value
		}),
		Setter(func(_ context.Context, v int64) {
			if sets != nil {
				*sets++
			}
			*value = v
		}),
	)
}

func TestCompileStructuralAndStaticErrors(t *testing.T) {
	tests := []struct {
		name string
		p    Program
	}{
		{"with before self", Schema(Case("x", With("1")))},
		{"unknown function", Schema(Case("x", Skip("missing()")))},
		{"skip non-bool", Schema(Case("x", Skip("1")))},
		{"wait non-duration", Schema(Case("x", Wait("1")))},
		{"duplicate root", Schema(Case("x"), Case("x"))},
		{"duplicate function", Schema(Func("f", func(context.Context, int64) (int64, error) { return 0, nil }), Func("f", func(context.Context, int64) (int64, error) { return 0, nil }), Case("x"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.p); err == nil {
				t.Fatal("expected compile error")
			}
		})
	}
}

func TestPendingSnapshotAndFinalSetterValue(t *testing.T) {
	x, y := int64(1), int64(10)
	xGets, yGets, xSets, ySets := 0, 0, 0, 0
	e, err := Compile(Schema(Case("x",
		Self("x"), Self("y"), With("y + x + x"), // the last Self wins
		Self("x"), With("y + 1"), With("x + y"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	s := Scope(intState("x", &x, &xGets, &xSets), intState("y", &y, &yGets, &ySets))
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if x != 25 || y != 12 {
		t.Fatalf("x,y = %d,%d; want 25,12", x, y)
	}
	if xGets != 1 || yGets != 1 {
		t.Fatalf("getter calls x=%d y=%d", xGets, yGets)
	}
	if xSets != 1 || ySets != 1 {
		t.Fatalf("setter calls x=%d y=%d", xSets, ySets)
	}
}

func TestErrorAndCancellationDiscardWholeSegment(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   Statement
	}{
		{"cel", With("1 / 0")},
		{"negative wait", Wait(`duration("-1s")`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, sets := int64(4), 0
			e, err := Compile(Schema(Case("x", Self("v"), With("v+1"), tc.op)))
			if err != nil {
				t.Fatal(err)
			}
			s := Scope(intState("v", &v, nil, &sets))
			if err := e.Do(context.Background(), s, "x"); err == nil {
				t.Fatal("expected error")
			}
			if v != 4 || sets != 0 {
				t.Fatalf("segment committed: v=%d setters=%d", v, sets)
			}
		})
	}
}

func TestCommitPreflightsAllTypesBeforeFirstSetter(t *testing.T) {
	a, b, sets := int64(1), int64(2), 0
	e, err := Compile(Schema(Case("x", Self("a"), With("a+1"), Self("b"), With(`"bad"`))))
	if err != nil {
		t.Fatal(err)
	}
	s := Scope(
		State("a", Getter(func(context.Context) int64 { return a }), Setter(func(_ context.Context, v int64) { sets++; a = v })),
		State("b", Getter(func(context.Context) int64 { return b }), Setter(func(_ context.Context, v int64) { sets++; b = v })),
	)
	if err := e.Do(context.Background(), s, "x"); err == nil {
		t.Fatal("expected assignment error")
	}
	if a != 1 || b != 2 || sets != 0 {
		t.Fatalf("partial commit: a=%d b=%d setters=%d", a, b, sets)
	}
}

func TestSkipScopesAndRootBoundary(t *testing.T) {
	v := int64(0)
	e, err := Compile(Schema(
		Case("root", Self("v"), With("v+1"), Case("a", Case("b", Skip("true"), With("1000")), With("v+10")), With("v+100")),
		Case("other", Self("v"), With("9999")),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := Scope(intState("v", &v, nil, nil))
	if err := e.Do(context.Background(), s, "root"); err != nil {
		t.Fatal(err)
	}
	if v != 111 {
		t.Fatalf("v=%d, nested skip escaped its case or crossed root", v)
	}
}

func TestWaitSegmentsRefreshSnapshotAndDoNotReplay(t *testing.T) {
	now := time.Unix(1, 0)
	v, gets, sets := int64(0), 0, 0
	e, err := Compile(Schema(Case("x", Self("v"), With("v+1"), Wait(`duration("0s")`), With("v+10"), Wait(`duration("1s")`), With("v+100"))))
	if err != nil {
		t.Fatal(err)
	}
	s := Scope(intState("v", &v, &gets, &sets))
	s.SetClock(func() time.Time { return now })
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if v != 1 || gets != 1 || sets != 1 {
		t.Fatalf("first segment v/gets/sets=%d/%d/%d", v, gets, sets)
	}
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if v != 11 || gets != 2 || sets != 2 {
		t.Fatalf("second segment v/gets/sets=%d/%d/%d", v, gets, sets)
	}
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if v != 11 || gets != 2 || sets != 2 {
		t.Fatal("early Do performed work")
	}
	now = now.Add(time.Second)
	if err := e.Do(context.Background(), s, "x"); err != nil {
		t.Fatal(err)
	}
	if v != 111 || gets != 3 || sets != 3 {
		t.Fatalf("resume v/gets/sets=%d/%d/%d", v, gets, sets)
	}
	if _, ok := s.continuations["x"]; ok {
		t.Fatal("completed continuation retained")
	}
}

func TestInvalidContinuationExecutesNothing(t *testing.T) {
	v, gets := int64(0), 0
	e, _ := Compile(Schema(Case("x", Self("v"), With("v+1"))))
	s := Scope(intState("v", &v, &gets, nil))
	s.engineVersion = e.version
	s.continuations["x"] = continuation{RootCase: "other", PC: 0, Version: e.version}
	if err := e.Do(context.Background(), s, "x"); err == nil {
		t.Fatal("expected invalid continuation error")
	}
	if v != 0 || gets != 0 {
		t.Fatal("invalid continuation executed operators")
	}
}

func TestScopesSerializeIndependently(t *testing.T) {
	var active, peak atomic.Int32
	e, _ := Compile(Schema(Case("x", Self("v"), With("slow(v)")), Func("slow", func(_ context.Context, v int64) (int64, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		return v + 1, nil
	})))
	wrap := func(v *int64) *Runtime {
		return Scope(State("v", Getter(func(context.Context) int64 {
			return *v
		}), Setter(func(_ context.Context, x int64) { *v = x })))
	}
	a, b := int64(0), int64(0)
	sa, sb := wrap(&a), wrap(&b)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = e.Do(context.Background(), sa, "x") }()
	go func() { defer wg.Done(); _ = e.Do(context.Background(), sb, "x") }()
	wg.Wait()
	if peak.Load() < 2 {
		t.Fatal("different scopes did not overlap")
	}

	active.Store(0)
	peak.Store(0)
	a = 0
	wg.Add(2)
	go func() { defer wg.Done(); _ = e.Do(context.Background(), sa, "x") }()
	go func() { defer wg.Done(); _ = e.Do(context.Background(), sa, "x") }()
	wg.Wait()
	if peak.Load() != 1 || a != 2 {
		t.Fatalf("same scope overlap=%d value=%d", peak.Load(), a)
	}
}

func TestCancelledBeforeCommit(t *testing.T) {
	v, sets := int64(0), 0
	ctx, cancel := context.WithCancel(context.Background())
	e, _ := Compile(Schema(Func("cancel", func(_ context.Context, x int64) (int64, error) { cancel(); return x, nil }), Case("x", Self("v"), With("v+1"), With("cancel(v)"))))
	s := Scope(intState("v", &v, nil, &sets))
	if err := e.Do(ctx, s, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if v != 0 || sets != 0 {
		t.Fatal("cancelled segment committed")
	}
}

func FuzzExecutionBoundaries(f *testing.F) {
	f.Add(uint8(1), uint8(2), uint8(3), false)
	f.Add(uint8(0), uint8(255), uint8(7), true)
	f.Fuzz(func(t *testing.T, da, db, dc uint8, skipNested bool) {
		deltas := []int64{int64(da % 20), int64(db % 20), int64(dc % 20)}
		ops := []Statement{Self("v"), With(fmt.Sprintf("v + %d", deltas[0])), Wait(`duration("0s")`)}
		if skipNested {
			ops = append(ops, Case("nested", Skip("true"), With("v + 1000000")))
		}
		ops = append(ops,
			Self("v"), With(fmt.Sprintf("v + %d", deltas[1])), Wait(`duration("0s")`),
			Self("v"), With(fmt.Sprintf("v + %d", deltas[2])),
		)
		e, err := Compile(Schema(
			Case("root", ops...),
			Case("other", Self("other"), With("other+1")),
		))
		if err != nil {
			t.Fatal(err)
		}
		v, other, gets, sets := int64(0), int64(0), 0, 0
		s := Scope(intState("v", &v, &gets, &sets), intState("other", &other, nil, nil))
		want := int64(0)
		for segment, delta := range deltas {
			if err := e.Do(context.Background(), s, "root"); err != nil {
				t.Fatal(err)
			}
			want += delta
			if v != want {
				t.Fatalf("segment %d replayed/skipped work: got %d want %d", segment, v, want)
			}
			if gets != segment+1 || sets != segment+1 {
				t.Fatalf("segment %d snapshot/write count %d/%d", segment, gets, sets)
			}
			if other != 0 {
				t.Fatal("unselected public case executed")
			}
		}
		if _, exists := s.continuations["root"]; exists {
			t.Fatal("root did not finish at its boundary")
		}
	})
}
