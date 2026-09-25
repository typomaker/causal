package main

import (
	"context"
	"testing"

	"causal"
)

func TestGoAndJSONProgramsProduceSameState(t *testing.T) {
	jsonDeclaration, err := jsonProgram()
	if err != nil {
		t.Fatal(err)
	}

	for name, program := range map[string]causal.Program{
		"Go":   goProgram(),
		"JSON": jsonDeclaration,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := execute(context.Background(), program)
			if err != nil {
				t.Fatal(err)
			}
			want := player{health: 75, energy: 17, score: 100}
			if got != want {
				t.Fatalf("state = %+v, want %+v", got, want)
			}
		})
	}
}

func TestExecuteReportsCompileError(t *testing.T) {
	_, err := execute(context.Background(), causal.New(causal.Case("broken", causal.With("1"))))
	if err == nil {
		t.Fatal("execute accepted an invalid program")
	}
}

func TestExecuteReportsRuntimeError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := execute(ctx, goProgram())
	if err == nil {
		t.Fatal("execute ignored context cancellation")
	}
}

func TestMain(t *testing.T) {
	main()
}
