package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"

	"causal"
)

//go:embed program.json
var jsonSource []byte

type player struct {
	health int64
	energy int64
	score  int64
}

func goProgram() causal.Program {
	return causal.New(
		causal.Symbol[int64]("health"),
		causal.Symbol[int64]("energy"),
		causal.Symbol[int64]("score"),

		causal.Case("game_turn",
			// This nested case atomically updates two variables.
			causal.Case("heal",
				causal.Self("health"),
				causal.With("health + 20"),
				causal.Self("energy"),
				causal.With("energy - 5"),
			),

			// This nested case atomically updates three variables.
			causal.Case("complete_quest",
				causal.Self("score"),
				causal.With("score + 100"),
				causal.Self("energy"),
				causal.With("energy + 2"),
				causal.Self("health"),
				causal.With("health + 5"),
			),
		),
	)
}

func jsonProgram() (causal.Program, error) {
	var program causal.Program
	err := json.Unmarshal(jsonSource, &program)
	return program, err
}

func execute(ctx context.Context, program causal.Program) (player, error) {
	state := player{health: 50, energy: 20, score: 0}
	runtime, err := program.Compile()
	if err != nil {
		return player{}, err
	}

	bindings := []causal.Binding{
		causal.Bind("health",
			causal.Getter(func(context.Context) int64 { return state.health }),
			causal.Setter(func(_ context.Context, value int64) { state.health = value }),
		),
		causal.Bind("energy",
			causal.Getter(func(context.Context) int64 { return state.energy }),
			causal.Setter(func(_ context.Context, value int64) { state.energy = value }),
		),
		causal.Bind("score",
			causal.Getter(func(context.Context) int64 { return state.score }),
			causal.Setter(func(_ context.Context, value int64) { state.score = value }),
		),
	}

	var scope causal.Scope
	if err := runtime.Do(ctx, &scope, "game_turn", bindings...); err != nil {
		return player{}, err
	}
	return state, nil
}

func main() {
	fromGo, err := execute(context.Background(), goProgram())
	if err != nil {
		log.Fatal(err)
	}

	declaration, err := jsonProgram()
	if err != nil {
		log.Fatal(err)
	}
	fromJSON, err := execute(context.Background(), declaration)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go:   health=%d energy=%d score=%d\n", fromGo.health, fromGo.energy, fromGo.score)
	fmt.Printf("JSON: health=%d energy=%d score=%d\n", fromJSON.health, fromJSON.energy, fromJSON.score)
}
