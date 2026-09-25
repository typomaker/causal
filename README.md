# causal

`causal` is a Go library for declarative, CEL-powered state transitions. A
`Program` is compiled once into an immutable `Runtime`; a zero-value `Scope`
stores only mutable execution state. Application values and functions are
provided as bindings on every `Do` call.

```go
program := causal.New(
    causal.Symbol[float64]("health"),
    causal.Symbol[float64]("damage"),
    causal.Case("attack",
        causal.Skip("damage <= 0"),
        causal.Self("health"),
        causal.With("max(0, health - damage)"),
        causal.Wait(`duration("3s")`),
    ),
)

runtime, err := program.Compile()
if err != nil {
    return err
}

var scope causal.Scope
err = runtime.Do(ctx, &scope, "attack",
    causal.Bind("health",
        causal.Getter(func(context.Context) float64 { return player.Health }),
        causal.Setter(func(_ context.Context, value float64) { player.Health = value }),
    ),
    causal.Bind("damage",
        causal.Getter(func(context.Context) float64 { return hit.Damage }),
    ),
)
```

Function symbols declare their complete compile-time signature. The runtime
implementation is supplied separately:

```go
causal.Symbol[
    func(context.Context, float64, float64) (float64, error)
]("calculate_damage")

causal.Bind("calculate_damage", calculateDamage)
```

Function signatures may omit `context.Context`, `error`, or both. Neither is
part of the CEL signature. Getters and setters intentionally have only these
forms:

```go
func(context.Context) T
func(context.Context, T)
```

`Do` validates the binding contract for the complete selected root case before
executing anything. This remains true when execution resumes after `Wait`.
Within a segment, reads are lazy and see staged writes. Writes commit at `Wait`
or root-case completion. Dependency propagation marks root cases ready but
never executes them.

## Declarative JSON and AST

Package `causal/ast` exposes the canonical declaration types: `Program`,
`Symbol`, `Case`, `Stmt`, `Self`, `With`, `Skip`, and `Wait`. Both `ast.Program`
and the ergonomic `causal.Program` wrapper work with `encoding/json`.

```json
{
  "attack": [
    { "self": "health" },
    { "with": "health - damage" }
  ]
}
```

JSON contains cases and statements only. Symbol contracts, compiled CEL,
bindings, functions, continuations, and other runtime metadata are deliberately
excluded. Symbol declarations therefore need to be composed in Go before a
decoded case document is compiled.

## Scope persistence

`Scope` serializes only its runtime version, readiness, and continuations:

```go
data, err := json.Marshal(&scope)

var restored causal.Scope
err = json.Unmarshal(data, &restored)
```

Bindings are always passed again to `Runtime.Do`. A restored continuation is
rejected when it belongs to a different compiled runtime.

## Tests and benchmarks

```sh
go test ./...
go test ./... -coverprofile=coverage.out
```
