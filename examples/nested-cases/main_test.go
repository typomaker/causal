package main

import (
	"context"
	"testing"

	"causal"
)

func TestCombinedProgram(t *testing.T) {
	program, err := combinedProgram()
	if err != nil {
		t.Fatal(err)
	}

	got, err := execute(context.Background(), program)
	if err != nil {
		t.Fatal(err)
	}
	want := player{health: 75, energy: 17, score: 100}
	if got != want {
		t.Fatalf("state = %+v, want %+v", got, want)
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
	program, err := combinedProgram()
	if err != nil {
		t.Fatal(err)
	}
	_, err = execute(ctx, program)
	if err == nil {
		t.Fatal("execute ignored context cancellation")
	}
}

func TestMain(t *testing.T) {
	main()
}
