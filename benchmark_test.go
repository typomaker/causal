package causal

import (
	"context"
	"testing"
	"time"
)

func BenchmarkDo(b *testing.B) {
	ctx := context.Background()
	plain := func(name string, statements ...Statement) {
		b.Run(name, func(b *testing.B) {
			runtime, err := New(Symbol[int64]("value"), Case("run", statements...)).Compile()
			if err != nil {
				b.Fatal(err)
			}
			value := int64(1)
			binding := Bind("value", &value)
			var scope Scope
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := runtime.Do(ctx, &scope, "run", binding); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	plain("read-only", Skip("value > 0"))
	plain("one With", Self("value"), With("value + 1"))
	plain("multiple With", Self("value"), With("value + 1"), With("value + 2"), With("value + 3"))
	plain("Skip", Skip("value < 0"), Self("value"), With("value + 1"))
	plain("nested Case", Self("value"), Case("nested", With("value + 1"), With("value + 2")), With("value + 3"))

	b.Run("function symbol", func(b *testing.B) {
		runtime, err := New(Symbol[int64]("value"), Symbol[func(int64) int64]("increment"), Case("run", Self("value"), With("increment(value)"))).Compile()
		if err != nil {
			b.Fatal(err)
		}
		value := int64(1)
		bindings := []Binding{Bind("value", &value), Bind("increment", func(v int64) int64 { return v + 1 })}
		var scope Scope
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := runtime.Do(ctx, &scope, "run", bindings...); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("context+error function", func(b *testing.B) {
		runtime, err := New(Symbol[int64]("value"), Symbol[func(context.Context, int64) (int64, error)]("increment"), Case("run", Self("value"), With("increment(value)"))).Compile()
		if err != nil {
			b.Fatal(err)
		}
		value := int64(1)
		bindings := []Binding{Bind("value", &value), Bind("increment", func(_ context.Context, v int64) (int64, error) { return v + 1, nil })}
		var scope Scope
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := runtime.Do(ctx, &scope, "run", bindings...); err != nil {
				b.Fatal(err)
			}
		}
	})

	benchmarkWait(b, ctx, false)
	benchmarkWait(b, ctx, true)
}

func benchmarkWait(b *testing.B, ctx context.Context, resume bool) {
	name := "Wait"
	if resume {
		name = "resume"
	}
	b.Run(name, func(b *testing.B) {
		runtime, err := New(Symbol[int64]("value"), Case("run", Self("value"), With("value + 1"), Wait(`duration("1s")`), With("value + 1"))).Compile()
		if err != nil {
			b.Fatal(err)
		}
		value := int64(1)
		binding := Bind("value", &value)
		now := time.Unix(1, 0)
		var continuationSeed continuation
		if resume {
			var seed Scope
			seed.SetClock(func() time.Time { return now })
			if err := runtime.Do(ctx, &seed, "run", binding); err != nil {
				b.Fatal(err)
			}
			continuationSeed = seed.continuations["run"]
			now = now.Add(time.Second)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var scope Scope
			if resume {
				scope = Scope{continuations: map[string]continuation{"run": continuationSeed}, clock: func() time.Time { return now }, runtimeVersion: runtime.version}
			} else {
				scope.SetClock(func() time.Time { return now })
			}
			if err := runtime.Do(ctx, &scope, "run", binding); err != nil {
				b.Fatal(err)
			}
		}
	})
}
