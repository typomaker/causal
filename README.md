# causal

`causal` is a Go library for describing state transitions as small declarative
cases. Expressions are written in CEL, while application state remains in
ordinary Go values.

## The problem it solves

Business transitions often start as straightforward assignments and gradually
accumulate conditions, shared subflows, delayed steps, and dependencies:

- one action needs to update several related values;
- the same subflow is reused by multiple actions;
- rules need to come from configuration instead of a Go deployment;
- a delayed action must resume without storing application objects in the
  workflow state;
- a rule must be checked before any partial state is written.

Scattering that logic across handlers makes execution order and partial writes
hard to reason about. `causal` separates the transition declaration from the
state bindings. A `Program` is compiled once into an immutable `Runtime`, and a
zero-value `Scope` stores only execution metadata.

```text
Go cases + JSON cases -> Program -> compiled Runtime
                                      |
                         Scope + bindings + case name
                                      |
                               application state
```

Within an execution segment, reads are lazy and writes are staged. Expressions
later in the segment see earlier staged values. All required bindings and all
result types are validated before setters are called. Staged writes are applied
at root-case completion or immediately before a `Wait`.

## Basic case

A case selects a target with `Self` and computes its next value with `With`:

```go
program := causal.New(
    causal.Symbol[int64]("balance"),
    causal.Symbol[int64]("amount"),
    causal.Case("deposit",
        causal.Self("balance"),
        causal.With("balance + amount"),
    ),
)

runtime, err := program.Compile()
if err != nil {
    return err
}

var scope causal.Scope
err = runtime.Do(ctx, &scope, "deposit",
    causal.Bind("balance",
        causal.Getter(func() int64 { return account.Balance }),
        causal.Setter(func(value int64) {
            account.Balance = value
        }),
    ),
    causal.Bind("amount",
        causal.Getter(func() int64 { return command.Amount }),
    ),
)
```

Only symbols required by the selected root case need bindings. A written symbol
requires both a getter and a setter; a read-only symbol requires only a getter.

## Updating several variables

Several `Self`/`With` pairs form one staged segment. If expression evaluation or
validation fails, none of the setters for that segment are called.

```go
causal.Case("complete_quest",
    causal.Self("score"),
    causal.With("score + 100"),
    causal.Self("energy"),
    causal.With("energy + 2"),
    causal.Self("health"),
    causal.With("health + 5"),
)
```

This is useful for transition-like updates of two or three related variables.
Setters should remain small and deterministic: they are invoked sequentially
after the whole segment has been validated.

## Nested and reusable cases

Cases can be nested directly in Go. A nested case is part of its root case and
cannot be executed independently:

```go
program := causal.New(
    causal.Symbol[int64]("health"),
    causal.Symbol[int64]("energy"),
    causal.Case("game_turn",
        causal.Case("heal",
            causal.Self("health"),
            causal.With("health + 20"),
            causal.Self("energy"),
            causal.With("energy - 5"),
        ),
    ),
)
```

JSON represents composition with named case references:

```json
{
  "@symbol": {
    "health": "int",
    "energy": "int"
  },
  "game_turn": [
    { "case": "heal" }
  ],
  "heal": [
    { "self": "health" },
    { "with": "health + 20" },
    { "self": "energy" },
    { "with": "energy - 5" }
  ]
}
```

`Skip` exits only the case in which it appears. This allows a nested case to be
conditional without cancelling the rest of its parent:

```go
causal.Case("heal",
    causal.Skip("energy < 5"),
    causal.Self("health"),
    causal.With("health + 20"),
    causal.Self("energy"),
    causal.With("energy - 5"),
)
```

## Combining Go and JSON

Programs are composable. For example, JSON can declare the symbols and the root
flow while Go supplies a reusable case:

```go
var fromJSON causal.Program
if err := json.Unmarshal(source, &fromJSON); err != nil {
    return err
}

fromGo := causal.New(
    causal.Case("heal",
        causal.Self("health"),
        causal.With("health + 20"),
        causal.Self("energy"),
        causal.With("energy - 5"),
    ),
)

combined := causal.New(fromJSON, fromGo)
runtime, err := combined.Compile()
```

References are resolved after composition, so a JSON case may refer to a case
defined in Go. Symbol and case names must remain unique in the combined program.
A complete, executable version is maintained as an
[`Example` test](examples/nested-cases/example_test.go).

## Conditions and delayed continuation

`Skip` conditionally exits the current case. `Wait` commits the current segment,
stores a continuation in `Scope`, and returns. Calling `Do` again resumes after
the delay has elapsed according to the scope clock.

```go
causal.Case("attack",
    causal.Skip("damage <= 0"),
    causal.Self("health"),
    causal.With("max(0, health - damage)"),
    causal.Wait(`duration("3s")`),
    causal.With("max(0, health - damage)"),
)
```

`Do` validates the binding contract for the complete root case before executing
anything, including when it resumes after `Wait`.

## Calling Go functions from CEL

Function symbols declare their complete Go signature. Their implementations are
provided as bindings at execution time:

```go
program := causal.New(
    causal.Symbol[float64]("health"),
    causal.Symbol[float64]("damage"),
    causal.Symbol[
        func(context.Context, float64, float64) (float64, error)
    ]("calculate_damage"),
    causal.Case("attack",
        causal.Self("health"),
        causal.With("calculate_damage(health, damage)"),
    ),
)

err := runtime.Do(ctx, &scope, "attack",
    healthBinding,
    damageBinding,
    causal.Bind("calculate_damage", calculateDamage),
)
```

Function signatures may omit `context.Context`, `error`, or both. Neither is
part of the CEL signature stored in JSON:

```json
{
  "@symbol": {
    "calculate_damage": "(double,double)double"
  }
}
```

Getters and setters intentionally support only these forms:

```go
func() T
func(T)
```

## Scope persistence

`Scope` serializes its runtime version, readiness, and continuations, but never
serializes application values or bindings:

```go
data, err := json.Marshal(&scope)
if err != nil {
    return err
}

var restored causal.Scope
if err := json.Unmarshal(data, &restored); err != nil {
    return err
}
```

Bindings are passed again on the next `Do` call. A restored continuation is
rejected if it belongs to a different compiled runtime.

## JSON and AST

Package `causal/ast` exposes the canonical declaration types: `Program`,
`Symbol`, `Case`, `Stmt`, `Self`, `With`, `Skip`, and `Wait`. Both `ast.Program`
and `causal.Program` support `encoding/json`.

The optional global `@symbol` object makes a JSON program independently
compilable. Value symbols use CEL type names: `bool`, `string`, `int`, `uint`,
`double`, and `duration`. JSON without `@symbol` can be composed with Go symbol
declarations through `causal.New`.

Compiled CEL, Go implementations, bindings, and continuations are never included
in the program JSON.

## Verification

```sh
go test ./...
go test ./... -coverprofile=coverage.out
golangci-lint run
```
