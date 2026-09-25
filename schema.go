package causal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	symbol       ast.Symbol
	kind         string
	function     bool
	args         []string
	result       string
	goType       reflect.Type
	context      bool
	returnsError bool
	err          error
}

func contractFor[T any](name string) (symbolContract, error) {
	t := reflect.TypeFor[T]()
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
