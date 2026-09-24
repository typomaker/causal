package causal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

type Program struct{ items []schemaItem }
type schemaItem interface{ schemaItem() }
type Declaration interface{ schemaItem() }
type Statement interface{ operation() }
type Block interface {
	Declaration
	Statement
}

// schemaItem lets independently built programs participate in Schema
// composition alongside declarations created by the Go API.
func (Program) schemaItem() {}

func Schema(items ...Declaration) Program {
	var internal []schemaItem
	for _, item := range items {
		if program, ok := item.(Program); ok {
			internal = append(internal, program.items...)
			continue
		}
		internal = append(internal, item)
	}
	return Program{items: internal}
}

type caseDef struct {
	name string
	ops  []operation
}

func (*caseDef) schemaItem() {}
func (*caseDef) operation()  {}
func Case(name string, ops ...Statement) Block {
	internal := make([]operation, len(ops))
	for i, op := range ops {
		internal[i] = op
	}
	return &caseDef{name, internal}
}

type funcDef struct {
	name, signature string
	args            []*cel.Type
	result          *cel.Type
	invoke          func(context.Context, []ref.Val) ref.Val
	err             error
}

func (*funcDef) schemaItem() {}

// Func registers statically adapted CEL functions.
// V1 supports the statically adapted signatures implemented by the package.
func Func(name string, fn any) Declaration {
	f := &funcDef{name: name}
	switch x := fn.(type) {
	case func(context.Context, float64, float64) (float64, error):
		f.binary("(double,double)double", cel.DoubleType, func(c context.Context, a, b ref.Val) ref.Val {
			av, e := as[float64](a)
			if e != nil {
				return celErr(e)
			}
			bv, e := as[float64](b)
			if e != nil {
				return celErr(e)
			}
			v, e := x(c, av, bv)
			if e != nil {
				return celErr(e)
			}
			return types.Double(v)
		})
	case func(context.Context, int64, int64) (int64, error):
		f.binary("(int,int)int", cel.IntType, func(c context.Context, a, b ref.Val) ref.Val {
			av, e := as[int64](a)
			if e != nil {
				return celErr(e)
			}
			bv, e := as[int64](b)
			if e != nil {
				return celErr(e)
			}
			v, e := x(c, av, bv)
			if e != nil {
				return celErr(e)
			}
			return types.Int(v)
		})
	case func(context.Context, float64, float64) float64:
		f.binary("(double,double)double", cel.DoubleType, func(c context.Context, a, b ref.Val) ref.Val {
			av, e := as[float64](a)
			if e != nil {
				return celErr(e)
			}
			bv, e := as[float64](b)
			if e != nil {
				return celErr(e)
			}
			return types.Double(x(c, av, bv))
		})
	case func(context.Context, int64, int64) int64:
		f.binary("(int,int)int", cel.IntType, func(c context.Context, a, b ref.Val) ref.Val {
			av, e := as[int64](a)
			if e != nil {
				return celErr(e)
			}
			bv, e := as[int64](b)
			if e != nil {
				return celErr(e)
			}
			return types.Int(x(c, av, bv))
		})
	case func(context.Context, int64) (int64, error):
		f.unary("(int)int", cel.IntType, func(c context.Context, a ref.Val) ref.Val {
			av, e := as[int64](a)
			if e != nil {
				return celErr(e)
			}
			v, e := x(c, av)
			if e != nil {
				return celErr(e)
			}
			return types.Int(v)
		})
	case func(context.Context, float64) (float64, error):
		f.unary("(double)double", cel.DoubleType, func(c context.Context, a ref.Val) ref.Val {
			av, e := as[float64](a)
			if e != nil {
				return celErr(e)
			}
			v, e := x(c, av)
			if e != nil {
				return celErr(e)
			}
			return types.Double(v)
		})
	default:
		f.err = fmt.Errorf("causal: Func %q has unsupported static signature", name)
	}
	return f
}
func (f *funcDef) unary(sig string, t *cel.Type, call func(context.Context, ref.Val) ref.Val) {
	f.signature = sig
	f.args = []*cel.Type{t}
	f.result = t
	f.invoke = func(c context.Context, a []ref.Val) ref.Val {
		if len(a) != 1 {
			return types.NewErr("wrong argument count")
		}
		return call(c, a[0])
	}
}
func (f *funcDef) binary(sig string, t *cel.Type, call func(context.Context, ref.Val, ref.Val) ref.Val) {
	f.signature = sig
	f.args = []*cel.Type{t, t}
	f.result = t
	f.invoke = func(c context.Context, a []ref.Val) ref.Val {
		if len(a) != 2 {
			return types.NewErr("wrong argument count")
		}
		return call(c, a[0], a[1])
	}
}
func as[T any](v ref.Val) (T, error) {
	var zero T
	x, ok := v.Value().(T)
	if !ok {
		return zero, fmt.Errorf("cannot use CEL %s as function argument", v.Type())
	}
	return x, nil
}
func celErr(err error) ref.Val { return types.NewErr("%s", err) }
func validateFunc(f *funcDef) error {
	if f.name == "" {
		return fmt.Errorf("causal: function name is empty")
	}
	return f.err
}

type operation interface{ operation() }
type selfOp struct{ name string }
type withOp struct{ expr string }
type skipOp struct{ expr string }
type waitOp struct{ expr string }
type caseRefOp struct{ name string }
type resolvedCaseRef struct{ def *caseDef }

func (selfOp) operation()          {}
func (withOp) operation()          {}
func (skipOp) operation()          {}
func (waitOp) operation()          {}
func (caseRefOp) operation()       {}
func (resolvedCaseRef) operation() {}
func Self(n string) Statement      { return selfOp{n} }
func With(e string) Statement      { return withOp{e} }
func Skip(e string) Statement      { return skipOp{e} }
func Wait(e string) Statement      { return waitOp{e} }

type operationJSON map[string]string

func (s Program) MarshalJSON() ([]byte, error) {
	out := map[string][]operationJSON{}
	seen := map[*caseDef]bool{}
	var addCase func(*caseDef) error
	addCase = func(c *caseDef) error {
		if seen[c] {
			return nil
		}
		seen[c] = true
		if _, exists := out[c.name]; exists {
			return fmt.Errorf("causal: duplicate case %q", c.name)
		}
		ops := make([]operationJSON, 0, len(c.ops))
		out[c.name] = ops
		for _, raw := range c.ops {
			switch x := raw.(type) {
			case selfOp:
				ops = append(ops, operationJSON{"self": x.name})
			case withOp:
				ops = append(ops, operationJSON{"with": x.expr})
			case skipOp:
				ops = append(ops, operationJSON{"skip": x.expr})
			case waitOp:
				ops = append(ops, operationJSON{"wait": x.expr})
			case caseRefOp:
				ops = append(ops, operationJSON{"case": x.name})
			case resolvedCaseRef:
				ops = append(ops, operationJSON{"case": x.def.name})
			case *caseDef:
				ops = append(ops, operationJSON{"case": x.name})
				if err := addCase(x); err != nil {
					return err
				}
			default:
				return fmt.Errorf("causal: unsupported operation %T", raw)
			}
		}
		out[c.name] = ops
		return nil
	}
	for _, i := range s.items {
		switch x := i.(type) {
		case *caseDef:
			if err := addCase(x); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(out)
}

func (s *Program) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("causal: cannot unmarshal Schema into nil Program")
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	if encoded == nil {
		return fmt.Errorf("causal schema must be a JSON object")
	}
	document := make(map[string][]json.RawMessage, len(encoded))
	for name, raw := range encoded {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("case %q must be an array of operators", name)
		}
		var operators []json.RawMessage
		if err := json.Unmarshal(raw, &operators); err != nil {
			return fmt.Errorf("case %q must be an array of operators: %w", name, err)
		}
		document[name] = operators
	}
	items := make([]schemaItem, 0, len(document))
	for name, raws := range document {
		if name == "" {
			return fmt.Errorf("causal: case name is empty")
		}
		ops := make([]operation, 0, len(raws))
		for _, raw := range raws {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return fmt.Errorf("case %q: invalid operator: %w", name, err)
			}
			if len(fields) != 1 {
				return fmt.Errorf("operator must contain exactly one operation")
			}
			for operator, value := range fields {
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					return fmt.Errorf("causal operator %q value must be string", operator)
				}
				switch operator {
				case "self":
					ops = append(ops, selfOp{text})
				case "with":
					ops = append(ops, withOp{text})
				case "skip":
					ops = append(ops, skipOp{text})
				case "wait":
					ops = append(ops, waitOp{text})
				case "case":
					ops = append(ops, caseRefOp{text})
				default:
					return fmt.Errorf("unknown causal operator %q", operator)
				}
			}
		}
		items = append(items, &caseDef{name: name, ops: ops})
	}
	s.items = items
	return nil
}
