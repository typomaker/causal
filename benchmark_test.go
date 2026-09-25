package causal_test

import (
	"context"
	"testing"

	"causal"
)

func BenchmarkCompile(b *testing.B) {
	schema := causal.Schema(
		causal.Func("clamp", func(_ context.Context, value, limit int64) (int64, error) {
			if value > limit {
				return limit, nil
			}
			return value, nil
		}),
		causal.Case("update",
			causal.Skip("!enabled"),
			causal.Self("value"),
			causal.With("clamp(value + delta, limit)"),
		),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := causal.Compile(schema); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDo(b *testing.B) {
	benchmarks := []struct {
		name   string
		schema causal.Program
		scope  func() *causal.Runtime
	}{
		{
			name: "single_write",
			schema: causal.Schema(causal.Case("run",
				causal.Self("value"),
				causal.With("value + 1"),
			)),
			scope: benchmarkIntScope,
		},
		{
			name: "four_staged_writes",
			schema: causal.Schema(causal.Case("run",
				causal.Self("value"),
				causal.With("value + 1"),
				causal.With("value + 2"),
				causal.With("value + 3"),
				causal.With("value + 4"),
			)),
			scope: benchmarkIntScope,
		},
		{
			name: "skip",
			schema: causal.Schema(causal.Case("run",
				causal.Skip("enabled"),
				causal.Self("value"),
				causal.With("value + 1"),
			)),
			scope: func() *causal.Runtime {
				return causal.Scope(causal.State("enabled",
					causal.Getter(func(context.Context) bool { return true }),
				))
			},
		},
		{
			name: "nested_case",
			schema: causal.Schema(causal.Case("run",
				causal.Self("value"),
				causal.Case("nested",
					causal.With("value + 1"),
					causal.With("value + 2"),
				),
				causal.With("value + 3"),
			)),
			scope: benchmarkIntScope,
		},
		{
			name: "custom_function",
			schema: causal.Schema(
				causal.Func("increment", func(_ context.Context, value int64) (int64, error) {
					return value + 1, nil
				}),
				causal.Case("run",
					causal.Self("value"),
					causal.With("increment(value)"),
				),
			),
			scope: benchmarkIntScope,
		},
	}

	ctx := context.Background()
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			engine, err := causal.Compile(benchmark.schema)
			if err != nil {
				b.Fatal(err)
			}
			runtime := benchmark.scope()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := engine.Do(ctx, runtime, "run"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkIntScope() *causal.Runtime {
	var value int64
	return causal.Scope(causal.State("value",
		causal.Getter(func(context.Context) int64 { return value }),
		causal.Setter(func(_ context.Context, next int64) { value = next }),
	))
}
