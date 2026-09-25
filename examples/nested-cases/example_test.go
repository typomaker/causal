package nestedcases_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"causal"
)

//go:embed program.json
var jsonSource []byte

func Example() {
	var fromJSON causal.Program
	if err := json.Unmarshal(jsonSource, &fromJSON); err != nil {
		panic(err)
	}

	fromGo := causal.New(
		causal.Case("heal",
			causal.Self("health"),
			causal.With("health + 20"),
			causal.Self("energy"),
			causal.With("energy - 5"),
		),
	)

	program := causal.New(fromJSON, fromGo)
	runtime, err := program.Compile()
	if err != nil {
		panic(err)
	}

	health, energy, score := int64(50), int64(20), int64(0)
	bindings := []causal.Binding{
		causal.Bind("health",
			causal.Getter(func() int64 { return health }),
			causal.Setter(func(value int64) { health = value }),
		),
		causal.Bind("energy",
			causal.Getter(func() int64 { return energy }),
			causal.Setter(func(value int64) { energy = value }),
		),
		causal.Bind("score",
			causal.Getter(func() int64 { return score }),
			causal.Setter(func(value int64) { score = value }),
		),
	}

	var scope causal.Scope
	if err := runtime.Do(context.Background(), &scope, "game_turn", bindings...); err != nil {
		panic(err)
	}

	fmt.Printf("health=%d energy=%d score=%d\n", health, energy, score)
	// Output:
	// health=75 energy=17 score=100
}
