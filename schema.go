package causal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"causal/ast"
)

// Program contains symbol declarations and cases before compilation. Programs
// created in Go and decoded from JSON can be combined with [New].
type Program struct {
	AST       ast.Program
	contracts []symbolContract
}

// Declaration is an item accepted by [New], such as a Symbol, Case, or Program.
type Declaration interface{}

// Statement is an operation accepted by [Case].
type Statement = ast.Stmt

// Block is a named Case that can be nested in another Case.
type Block = ast.Case
type symbolDeclaration struct{ contract symbolContract }

// New builds a program from symbol declarations, cases, and other programs.
// For example:
//
//	program := causal.New(
//		causal.Symbol[int64]("health"),
//		causal.Case("heal", causal.Self("health"), causal.With("health + 10")),
//	)
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
		case ast.Program:
			p.AST.Cases = append(p.AST.Cases, x.Cases...)
			p.AST.Symbols = append(p.AST.Symbols, x.Symbols...)
			for _, symbol := range x.Symbols {
				contract, err := contractFromAST(symbol)
				contract.err = err
				p.contracts = append(p.contracts, contract)
			}
		}
	}
	return p
}

// Symbol declares a value or function available to CEL expressions. Supported
// value types include bool, string, int64, uint64, float64, and time.Duration:
//
//	health := causal.Symbol[int64]("health")
//	lookup := causal.Symbol[func(context.Context, string) (int64, error)]("lookup")
func Symbol[T any](name string) Declaration {
	c, err := contractFor[T](name)
	c.err = err
	return symbolDeclaration{c}
}

// Case declares a named sequence of statements. Cases may be nested to reuse a
// conditional subflow:
//
//	heal := causal.Case("heal", causal.Self("health"), causal.With("health + 10"))
func Case(name string, statements ...Statement) Block {
	return ast.Case{Name: name, Statements: statements}
}

// Self selects the value symbol assigned by subsequent With statements:
//
//	causal.Self("health")
func Self(name string) Statement { return ast.Self{Symbol: name} }

// With evaluates a CEL expression and stages its result for the current Self:
//
//	causal.With("max(0, health - damage)")
func With(expr string) Statement { return ast.With{Expression: expr} }

// Skip exits the current Case when its CEL expression evaluates to true:
//
//	causal.Skip("energy < 5")
func Skip(expr string) Statement { return ast.Skip{Expression: expr} }

// Wait commits staged values and suspends the root Case for a CEL duration:
//
//	causal.Wait(`duration("3s")`)
func Wait(expr string) Statement { return ast.Wait{Expression: expr} }

// Compile validates the program and creates an immutable Runtime:
//
//	runtime, err := program.Compile()
func (p Program) Compile() (*Runtime, error) { return compile(p) }

// MarshalJSON encodes the program's canonical AST representation:
//
//	data, err := json.Marshal(program)
func (p Program) MarshalJSON() ([]byte, error) { return json.Marshal(p.AST) }

// UnmarshalJSON decodes a canonical JSON program so it can be compiled or
// combined with Go declarations:
//
//	var program causal.Program
//	err := json.Unmarshal(data, &program)
func (p *Program) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("causal: cannot unmarshal into nil Program")
	}
	if err := json.Unmarshal(data, &p.AST); err != nil {
		return err
	}
	p.contracts = make([]symbolContract, 0, len(p.AST.Symbols))
	for _, symbol := range p.AST.Symbols {
		contract, err := contractFromAST(symbol)
		if err != nil {
			return err
		}
		p.contracts = append(p.contracts, contract)
	}
	return nil
}

type symbolContract struct {
	symbol       ast.Symbol
	kind         string
	function     bool
	args         []string
	result       string
	goType       reflect.Type
	context      bool
	returnsError bool
	nativeArgs   []reflect.Type
	err          error
}

func contractFor[T any](name string) (symbolContract, error) {
	return contractForType(name, reflect.TypeFor[T]())
}

func contractForType(name string, t reflect.Type) (symbolContract, error) {
	c := symbolContract{symbol: ast.Symbol{Name: name}, goType: t}
	if t.Kind() != reflect.Func {
		kind, ok := kindForType(t)
		if !ok {
			return c, fmt.Errorf("causal: Symbol %q has unsupported static type %v", name, t)
		}
		c.kind = kind
	} else {
		if t.IsVariadic() {
			return c, fmt.Errorf("causal: function Symbol %q must not be variadic", name)
		}
		c.function = true
		first := 0
		if t.NumIn() > 0 && t.In(0) == contextType {
			c.context = true
			first = 1
		}
		for i := first; i < t.NumIn(); i++ {
			kind, ok := kindForType(t.In(i))
			if !ok {
				return c, fmt.Errorf("causal: function Symbol %q argument %d has unsupported type %v", name, i-first, t.In(i))
			}
			c.args = append(c.args, kind)
			c.nativeArgs = append(c.nativeArgs, t.In(i))
		}
		if t.NumOut() != 1 && t.NumOut() != 2 {
			return c, fmt.Errorf("causal: function Symbol %q must return R or (R, error)", name)
		}
		kind, ok := kindForType(t.Out(0))
		if !ok {
			return c, fmt.Errorf("causal: function Symbol %q result has unsupported type %v", name, t.Out(0))
		}
		c.result = kind
		if t.NumOut() == 2 {
			if t.Out(1) != errorType {
				return c, fmt.Errorf("causal: function Symbol %q second result must be error", name)
			}
			c.returnsError = true
		}
	}
	c.symbol.Type, c.symbol.Function, c.symbol.Arguments, c.symbol.Result = c.kind, c.function, c.args, c.result
	return c, nil
}

func contractFromAST(symbol ast.Symbol) (symbolContract, error) {
	c := symbolContract{symbol: symbol, kind: symbol.Type, function: symbol.Function, args: append([]string(nil), symbol.Arguments...), result: symbol.Result}
	if symbol.Name == "" {
		return c, fmt.Errorf("causal: symbol name is empty")
	}
	if symbol.Function {
		for i, kind := range c.args {
			if !validKind(kind) {
				return c, fmt.Errorf("causal: function Symbol %q argument %d has unsupported CEL type %q", symbol.Name, i, kind)
			}
		}
		if !validKind(c.result) {
			return c, fmt.Errorf("causal: function Symbol %q result has unsupported CEL type %q", symbol.Name, c.result)
		}
		c.kind = ""
	} else if !validKind(c.kind) {
		return c, fmt.Errorf("causal: Symbol %q has unsupported CEL type %q", symbol.Name, c.kind)
	}
	return c, nil
}

func validKind(kind string) bool {
	switch kind {
	case "bool", "string", "int", "uint", "double", "duration":
		return true
	}
	return false
}

var contextType = reflect.TypeFor[context.Context]()
var errorType = reflect.TypeFor[error]()
var durationType = reflect.TypeFor[time.Duration]()

func kindForType(t reflect.Type) (string, bool) {
	switch t {
	case reflect.TypeFor[bool]():
		return "bool", true
	case reflect.TypeFor[string]():
		return "string", true
	case reflect.TypeFor[int64]():
		return "int", true
	case reflect.TypeFor[uint64]():
		return "uint", true
	case reflect.TypeFor[float64]():
		return "double", true
	case durationType:
		return "duration", true
	}
	return "", false
}
