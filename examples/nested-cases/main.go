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

func goCases() causal.Program {
	return causal.New(
		// game_turn in program.json references this Go case.
		causal.Case("heal",
			causal.Self("health"),
			causal.With("health + 20"),
			causal.Self("energy"),
			causal.With("energy - 5"),
		),
	)
}

func jsonProgram() (causal.Program, error) {
	var program causal.Program
	err := json.Unmarshal(jsonSource, &program)
	return program, err
}

func combinedProgram() (causal.Program, error) {
	fromJSON, err := jsonProgram()
	if err != nil {
		return causal.Program{}, err
	}
	return causal.New(fromJSON, goCases()), nil
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
	program, err := combinedProgram()
	if err != nil {
		log.Fatal(err)
	}
	state, err := execute(context.Background(), program)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Combined: health=%d energy=%d score=%d\n", state.health, state.energy, state.score)
}
