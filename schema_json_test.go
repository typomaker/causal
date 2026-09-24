package causal_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"causal"
)

func decodeSchema(t *testing.T, source string) causal.Program {
	t.Helper()
	var schema causal.Program
	if err := json.Unmarshal([]byte(source), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestSchemaJSONRoundTripAndOperators(t *testing.T) {
	source := `{
		"combat.attack":[
			{"self":"health"},
			{"with":"health - 1"},
			{"skip":"health <= 0"},
			{"wait":"duration(\"1s\")"},
			{"case":"combat.recover"}
		],
		"combat.recover":[{"self":"health"},{"with":"health + 1"}],
		"noop":[]
	}`
	schema := decodeSchema(t, source)
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var restored causal.Program
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(reencoded) {
		t.Fatalf("round trip changed declaration:\n%s\n%s", encoded, reencoded)
	}
	ordered := `[{"self":"health"},{"with":"health - 1"},{"skip":"health \u003c= 0"},{"wait":"duration(\"1s\")"},{"case":"combat.recover"}]`
	if !strings.Contains(string(encoded), ordered) {
		t.Fatalf("operator order changed: %s", encoded)
	}
}

func TestSchemaJSONEmptyAndDeterministic(t *testing.T) {
	empty := decodeSchema(t, `{}`)
	data, err := json.Marshal(empty)
	if err != nil || string(data) != `{}` {
		t.Fatalf("empty = %s, %v", data, err)
	}

	schema := decodeSchema(t, `{"z":[],"a":[],"m":[]}`)
	a, _ := json.Marshal(schema)
	b, _ := json.Marshal(schema)
	if string(a) != string(b) || string(a) != `{"a":[],"m":[],"z":[]}` {
		t.Fatalf("non-deterministic output: %s / %s", a, b)
	}
	indented, err := json.MarshalIndent(decodeSchema(t, `{"x":[{"skip":"!alive"},{"case":"y"}],"y":[]}`), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indented), "\n    {\n      \"skip\": \"!alive\"\n    }") {
		t.Fatalf("unreadable MarshalIndent:\n%s", indented)
	}
}

func TestSchemaJSONDecodeErrors(t *testing.T) {
	tests := []struct{ name, source, want string }{
		{"invalid JSON", `{`, "unexpected end"},
		{"unknown operator", `{"x":[{"waait":"x"}]}`, `unknown causal operator "waait"`},
		{"multiple operations", `{"x":[{"self":"x","with":"1"}]}`, "operator must contain exactly one operation"},
		{"no operations", `{"x":[{}]}`, "operator must contain exactly one operation"},
		{"wrong value", `{"x":[{"wait":5}]}`, `operator "wait" value must be string`},
		{"invalid case", `{"x":{}}`, "must be an array"},
		{"null case", `{"x":null}`, "must be an array"},
		{"empty name", `{"":[]}`, "case name is empty"},
		{"top-level array", `[]`, "cannot unmarshal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var schema causal.Program
			err := json.Unmarshal([]byte(tc.source), &schema)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestSchemaJSONReferencesAndComposition(t *testing.T) {
	attack := decodeSchema(t, `{"attack":[{"case":"execute"}]}`)
	execute := decodeSchema(t, `{"execute":[{"self":"ammo"},{"with":"ammo - 1"}]}`)
	schema := causal.Schema(attack, execute)
	engine, err := causal.Compile(schema)
	if err != nil {
		t.Fatal(err)
	}
	ammo := int64(2)
	scope := causal.Scope(causal.State("ammo",
		causal.Getter(func(context.Context) int64 { return ammo }),
		causal.Setter(func(_ context.Context, value int64) { ammo = value }),
	))
	if err := engine.Do(context.Background(), scope, "attack"); err != nil {
		t.Fatal(err)
	}
	if ammo != 1 {
		t.Fatalf("ammo = %d, want 1", ammo)
	}
}

func TestSchemaJSONCompileErrors(t *testing.T) {
	tests := []struct {
		name   string
		schema causal.Program
		want   string
	}{
		{"duplicate", causal.Schema(decodeSchema(t, `{"x":[]}`), decodeSchema(t, `{"x":[]}`)), `duplicate case "x"`},
		{"missing", decodeSchema(t, `{"attack":[{"case":"missing"}]}`), `case "attack": unknown case "missing"`},
		{"recursive", decodeSchema(t, `{"a":[{"case":"b"}],"b":[{"case":"a"}]}`), "recursive case reference: a -> b -> a"},
		{"invalid CEL", decodeSchema(t, `{"x":[{"skip":"("}]}`), "parse"},
		{"with without self", decodeSchema(t, `{"x":[{"with":"1"}]}`), "has no preceding Self"},
		{"unknown function", decodeSchema(t, `{"x":[{"skip":"missing()"}]}`), "undeclared reference"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := causal.Compile(tc.schema)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestSchemaJSONAndGoDeclarationsComposition(t *testing.T) {
	decoded := decodeSchema(t, `{"x":[{"self":"v"},{"with":"twice(v)"}]}`)
	schema := causal.Schema(decoded, causal.Func("twice", func(_ context.Context, v int64) (int64, error) { return v * 2, nil }))
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "(int)int") || !strings.Contains(string(data), `"with":"twice(v)"`) {
		t.Fatalf("function declaration leaked or expression lost: %s", data)
	}
	if _, err := causal.Compile(schema); err != nil {
		t.Fatal(err)
	}
}
