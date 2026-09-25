// Package ast defines the canonical, serializable causal program representation.
package ast

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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

func (p Program) MarshalJSON() ([]byte, error) {
	out := make(map[string]any)
	if len(p.Symbols) > 0 {
		symbols := make(map[string]string, len(p.Symbols))
		for _, symbol := range p.Symbols {
			if symbol.Name == "" {
				return nil, fmt.Errorf("causal: symbol name is empty")
			}
			if _, exists := symbols[symbol.Name]; exists {
				return nil, fmt.Errorf("causal: duplicate symbol %q", symbol.Name)
			}
			signature, err := formatSymbolSignature(symbol)
			if err != nil {
				return nil, err
			}
			symbols[symbol.Name] = signature
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
			var declarations map[string]string
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
				symbol, err := parseSymbolSignature(symbolName, declarations[symbolName])
				if err != nil {
					return err
				}
				symbols = append(symbols, symbol)
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

func formatSymbolSignature(symbol Symbol) (string, error) {
	if !symbol.Function {
		if strings.TrimSpace(symbol.Type) == "" {
			return "", fmt.Errorf("causal: symbol %q type is empty", symbol.Name)
		}
		return symbol.Type, nil
	}
	if strings.TrimSpace(symbol.Result) == "" {
		return "", fmt.Errorf("causal: function symbol %q result is empty", symbol.Name)
	}
	for i, argument := range symbol.Arguments {
		if strings.TrimSpace(argument) == "" {
			return "", fmt.Errorf("causal: function symbol %q argument %d is empty", symbol.Name, i)
		}
	}
	return "(" + strings.Join(symbol.Arguments, ",") + ")" + symbol.Result, nil
}

func parseSymbolSignature(name, signature string) (Symbol, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return Symbol{}, fmt.Errorf("causal: symbol %q signature is empty", name)
	}
	if !strings.HasPrefix(signature, "(") {
		if strings.ContainsAny(signature, "(),") {
			return Symbol{}, fmt.Errorf("causal: symbol %q has invalid value signature %q", name, signature)
		}
		return Symbol{Name: name, Type: signature}, nil
	}
	close := strings.IndexByte(signature, ')')
	if close < 0 || close == len(signature)-1 || strings.Contains(signature[close+1:], ")") {
		return Symbol{}, fmt.Errorf("causal: function symbol %q has invalid signature %q", name, signature)
	}
	result := strings.TrimSpace(signature[close+1:])
	if result == "" || strings.ContainsAny(result, "(),") {
		return Symbol{}, fmt.Errorf("causal: function symbol %q has invalid result %q", name, result)
	}
	var arguments []string
	inside := strings.TrimSpace(signature[1:close])
	if inside != "" {
		for i, argument := range strings.Split(inside, ",") {
			argument = strings.TrimSpace(argument)
			if argument == "" || strings.ContainsAny(argument, "()") {
				return Symbol{}, fmt.Errorf("causal: function symbol %q has invalid argument %d", name, i)
			}
			arguments = append(arguments, argument)
		}
	}
	return Symbol{Name: name, Function: true, Arguments: arguments, Result: result}, nil
}
