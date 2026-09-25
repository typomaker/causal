# causal

`causal` is a Go library for declarative, CEL-powered state transitions. A schema is compiled once into an immutable `Engine`; each `Runtime` binds CEL names to application memory and stores readiness and suspended execution state.

```go
engine, err := causal.Compile(causal.Schema(
    causal.Case("hit",
        causal.Skip("!alive"),
        causal.Self("health"),
        causal.With("max(0, health - damage)"),
    ),
))

scope := causal.Scope(
    causal.State("health",
        causal.Getter(func(context.Context) int64 { return player.Health }),
        causal.Setter(func(_ context.Context, value int64) { player.Health = value }),
    ),
    causal.State("damage", causal.Getter(func(context.Context) int64 { return 10 })),
    causal.State("alive", causal.Getter(func(context.Context) bool { return player.Alive })),
)

err = engine.Do(ctx, scope, "hit")
```

Within a segment, reads are lazy and see staged writes. Writes are applied only at `Wait` or case completion. `Wait` stores a continuation; calling `Do` before its deadline is a no-op, and calling it afterward resumes at the next instruction. Dependency propagation only marks cases ready; it never runs them.

`Getter` and `Setter` are statically typed, non-error-returning in-memory accessors. State value types are checked when CEL results are assigned. `Runtime.SetClock` can supply a deterministic clock function for simulations and tests.

## Declarative JSON

`Program` works directly with `encoding/json`. A document maps case names to
ordered operator arrays:

```json
{
  "combat.attack": [
    { "skip": "!alive" },
    { "case": "combat.execute" }
  ],
  "combat.execute": [
    { "self": "health" },
    { "with": "health - damage" }
  ]
}
```

```go
var combat causal.Program
err := json.Unmarshal(data, &combat)

schema := causal.Schema(
    combat,
    causal.Func("damage", damage),
)
engine, err := causal.Compile(schema)
```

Documents can be decoded independently and combined with `Schema`; named case
references are resolved only after composition. JSON contains declarations
only: functions, State bindings, compiled CEL, continuations, and other runtime
metadata are excluded. Loading files or database records remains the
application's responsibility.

## Runtime persistence

`Engine` JSON contains metadata only. A scope serializes only its engine version, readiness, and continuations:

```go
data, err := json.Marshal(scope)
restored := causal.Scope(
    causal.State("health", causal.Getter(getHealth), causal.Setter(setHealth)),
)
err = json.Unmarshal(data, restored)
```

Restored continuations are rejected if their engine version differs. Application state itself remains the application's persistence responsibility.

Because the specified API declares State bindings only in `Scope`, `Compile(schema)` cannot know their Go types or whether they have setters. CEL syntax, known functions, `Skip`/`Wait` types, and structural errors are checked at compile time; missing bindings, read-only targets, and dynamic State conversion are validated by `Do` before commit.

Writing the same value is deliberately treated as a change: its setter runs and
dependent root cases become ready. Equality is not consulted, so this policy is
independent of the State's Go type.

Getters, setters, and successful `Func` implementations are application
callbacks and must not panic. A panic is not converted into a causal error and
propagates to the caller. They must also obey the callback contract: getters and
setters are in-memory accessors, while a `Func` must not perform side effects
that would need rollback. Causal rollback covers staged State writes; it cannot
undo effects performed inside callbacks.

## Benchmarks

Run the compilation and execution benchmarks with allocation statistics:

```sh
go test -run '^$' -bench . -benchmem
```

Use `-count` and `benchstat` when comparing two revisions to reduce measurement
noise. For example, save each revision with `-count 10` and compare the output
files with `benchstat before.txt after.txt`.
