package ast

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONAllStatementsAndErrors(t *testing.T) {
	p := Program{Cases: []Case{{Name: "x", Statements: []Stmt{Self{"v"}, With{"v+1"}, Skip{"false"}, Wait{`duration("1s")`}, CaseRef{"y"}, Case{Name: "z"}}}, {Name: "y"}}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var q Program
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Cases) != 3 {
		t.Fatalf("cases=%d: %s", len(q.Cases), b)
	}
	bad := []string{`null`, `[]`, `{"x":null}`, `{"x":{}}`, `{"x":[{}]}`, `{"x":[{"self":1}]}`, `{"x":[{"bad":"x"}]}`, `{"x":[{"self":"x","with":"x"}]}`, `{"":[]}`}
	for _, source := range bad {
		var v Program
		if json.Unmarshal([]byte(source), &v) == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	var nilProgram *Program
	if json.Unmarshal([]byte(`{}`), nilProgram) == nil {
		t.Fatal("accepted nil receiver")
	}
	if _, err := json.Marshal(Program{Cases: []Case{{Name: "x", Statements: []Stmt{fakeStmt{}}}}}); err == nil {
		t.Fatal("accepted unsupported stmt")
	}
	if _, err := json.Marshal(Program{Cases: []Case{{Name: "x"}, {Name: "x"}}}); err == nil {
		t.Fatal("accepted duplicate")
	}
	if !strings.Contains(string(b), `"case":"z"`) {
		t.Fatal(string(b))
	}
}

type fakeStmt struct{}

func (fakeStmt) isStmt() {}
