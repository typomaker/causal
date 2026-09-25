// Package ast defines the canonical, serializable causal program representation.
package ast

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

type Program struct {
	Symbols []Symbol `json:"-"`
	Cases   []Case   `json:"-"`
}
type Symbol struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Function  bool     `json:"function,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	Result    string   `json:"result,omitempty"`
}
type Stmt interface{ isStmt() }
type Case struct {
	Name       string
	Statements []Stmt
}
type Self struct{ Symbol string }
type With struct{ Expression string }
type Skip struct{ Expression string }
type Wait struct{ Expression string }
type CaseRef struct{ Name string }

func (Case) isStmt()    {}
func (Self) isStmt()    {}
func (With) isStmt()    {}
func (Skip) isStmt()    {}
func (Wait) isStmt()    {}
func (CaseRef) isStmt() {}

type stmtJSON map[string]string
type symbolJSON struct {
	Type      string   `json:"type,omitempty"`
	Arguments []string `json:"args,omitempty"`
	Result    string   `json:"result,omitempty"`
}

func (p Program) MarshalJSON() ([]byte, error) {
	out := make(map[string]any)
	if len(p.Symbols) > 0 {
		symbols := make(map[string]symbolJSON, len(p.Symbols))
		for _, symbol := range p.Symbols {
			if symbol.Name == "" {
				return nil, fmt.Errorf("causal: symbol name is empty")
			}
			if _, exists := symbols[symbol.Name]; exists {
				return nil, fmt.Errorf("causal: duplicate symbol %q", symbol.Name)
			}
			if symbol.Function {
				symbols[symbol.Name] = symbolJSON{Arguments: append([]string{}, symbol.Arguments...), Result: symbol.Result}
			} else {
				symbols[symbol.Name] = symbolJSON{Type: symbol.Type}
			}
		}
		out["@symbol"] = symbols
	}
	seen := make(map[string]bool)
	var add func(Case) error
	add = func(c Case) error {
		if seen[c.Name] {
			return fmt.Errorf("causal: duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		ops := make([]stmtJSON, 0, len(c.Statements))
		for _, raw := range c.Statements {
			switch x := raw.(type) {
			case Self:
				ops = append(ops, stmtJSON{"self": x.Symbol})
			case With:
				ops = append(ops, stmtJSON{"with": x.Expression})
			case Skip:
				ops = append(ops, stmtJSON{"skip": x.Expression})
			case Wait:
				ops = append(ops, stmtJSON{"wait": x.Expression})
			case CaseRef:
				ops = append(ops, stmtJSON{"case": x.Name})
			case Case:
				ops = append(ops, stmtJSON{"case": x.Name})
				if err := add(x); err != nil {
					return err
				}
			default:
				return fmt.Errorf("causal: unsupported statement %T", raw)
			}
		}
		out[c.Name] = ops
		return nil
	}
	for _, c := range p.Cases {
		if err := add(c); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}
func (p *Program) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("causal: cannot unmarshal into nil ast.Program")
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	if encoded == nil {
		return fmt.Errorf("causal program must be a JSON object")
	}
	names := make([]string, 0, len(encoded))
	for name := range encoded {
		names = append(names, name)
	}
	sort.Strings(names)
	cases := make([]Case, 0, len(names))
	var symbols []Symbol
	for _, name := range names {
		if name == "@symbol" {
			var declarations map[string]symbolJSON
			if err := json.Unmarshal(encoded[name], &declarations); err != nil || declarations == nil {
				return fmt.Errorf("causal: @symbol must be an object")
			}
			symbolNames := make([]string, 0, len(declarations))
			for symbolName := range declarations {
				symbolNames = append(symbolNames, symbolName)
			}
			sort.Strings(symbolNames)
			for _, symbolName := range symbolNames {
				if symbolName == "" {
					return fmt.Errorf("causal: symbol name is empty")
				}
				declaration := declarations[symbolName]
				function := declaration.Result != "" || declaration.Arguments != nil
				if function == (declaration.Type != "") {
					return fmt.Errorf("causal: symbol %q must declare either type or function signature", symbolName)
				}
				symbols = append(symbols, Symbol{Name: symbolName, Type: declaration.Type, Function: function, Arguments: declaration.Arguments, Result: declaration.Result})
			}
			continue
		}
		if name == "" {
			return fmt.Errorf("causal: case name is empty")
		}
		raw := encoded[name]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("case %q must be an array of statements", name)
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("case %q must be an array of statements: %w", name, err)
		}
		c := Case{Name: name, Statements: make([]Stmt, 0, len(entries))}
		for _, entry := range entries {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(entry, &fields); err != nil {
				return fmt.Errorf("case %q: invalid statement: %w", name, err)
			}
			if len(fields) != 1 {
				return fmt.Errorf("statement must contain exactly one operation")
			}
			for op, value := range fields {
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					return fmt.Errorf("causal operation %q value must be string", op)
				}
				switch op {
				case "self":
					c.Statements = append(c.Statements, Self{Symbol: text})
				case "with":
					c.Statements = append(c.Statements, With{Expression: text})
				case "skip":
					c.Statements = append(c.Statements, Skip{Expression: text})
				case "wait":
					c.Statements = append(c.Statements, Wait{Expression: text})
				case "case":
					c.Statements = append(c.Statements, CaseRef{Name: text})
				default:
					return fmt.Errorf("unknown causal operation %q", op)
				}
			}
		}
		cases = append(cases, c)
	}
	p.Symbols, p.Cases = symbols, cases
	return nil
}
