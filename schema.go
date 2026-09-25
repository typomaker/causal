package causal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"causal/ast"
)

type Program struct {
	AST       ast.Program
	contracts []symbolContract
}
type Declaration interface{}
type Statement = ast.Stmt
type Block = ast.Case
type symbolDeclaration struct{ contract symbolContract }

func New(items ...Declaration) Program {
	p := Program{}
	for _, item := range items {
		switch x := item.(type) {
		case symbolDeclaration:
			p.contracts = append(p.contracts, x.contract)
			p.AST.Symbols = append(p.AST.Symbols, x.contract.symbol)
		case ast.Case:
			p.AST.Cases = append(p.AST.Cases, x)
		case Program:
			p.AST.Cases = append(p.AST.Cases, x.AST.Cases...)
			p.AST.Symbols = append(p.AST.Symbols, x.AST.Symbols...)
			p.contracts = append(p.contracts, x.contracts...)
		}
	}
	return p
}

func Symbol[T any](name string) Declaration {
	c, err := contractFor[T](name)
	c.err = err
	return symbolDeclaration{c}
}
func Case(name string, statements ...Statement) Block {
	return ast.Case{Name: name, Statements: statements}
}
func Self(name string) Statement               { return ast.Self{Symbol: name} }
func With(expr string) Statement               { return ast.With{Expression: expr} }
func Skip(expr string) Statement               { return ast.Skip{Expression: expr} }
func Wait(expr string) Statement               { return ast.Wait{Expression: expr} }
func (p Program) Compile() (*Runtime, error)   { return compile(p) }
func (p Program) MarshalJSON() ([]byte, error) { return json.Marshal(p.AST) }
func (p *Program) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("causal: cannot unmarshal into nil Program")
	}
	p.contracts = nil
	return json.Unmarshal(data, &p.AST)
}

type symbolContract struct {
	symbol   ast.Symbol
	kind     string
	function bool
	args     []string
	result   string
	err      error
}

func contractFor[T any](name string) (symbolContract, error) {
	var z T
	c := symbolContract{symbol: ast.Symbol{Name: name}}
	switch any(z).(type) {
	case bool:
		c.kind = "bool"
	case string:
		c.kind = "string"
	case int64:
		c.kind = "int"
	case uint64:
		c.kind = "uint"
	case float64:
		c.kind = "double"
	case time.Duration:
		c.kind = "duration"
	case func(float64, float64) float64, func(context.Context, float64, float64) float64, func(float64, float64) (float64, error), func(context.Context, float64, float64) (float64, error):
		c.function, c.args, c.result = true, []string{"double", "double"}, "double"
	case func(int64) int64, func(context.Context, int64) int64, func(int64) (int64, error), func(context.Context, int64) (int64, error):
		c.function, c.args, c.result = true, []string{"int"}, "int"
	case func(int64, int64) int64, func(context.Context, int64, int64) int64, func(int64, int64) (int64, error), func(context.Context, int64, int64) (int64, error):
		c.function, c.args, c.result = true, []string{"int", "int"}, "int"
	default:
		return c, fmt.Errorf("causal: Symbol %q has unsupported static type %T", name, z)
	}
	c.symbol.Type, c.symbol.Function, c.symbol.Arguments, c.symbol.Result = c.kind, c.function, c.args, c.result
	return c, nil
}
