package causal

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// Binding supplies the runtime implementation of one Symbol.
type Binding struct {
	name      string
	value     reflect.Value
	valueType reflect.Type
	function  any
	err       error
}

// Bind associates a value Symbol with a non-nil pointer, or a function Symbol
// with a function of its declared signature.
func Bind(name string, implementation any) Binding {
	b := Binding{name: name}
	t := reflect.TypeOf(implementation)
	if t == nil {
		b.err = fmt.Errorf("implementation is nil")
		return b
	}
	switch t.Kind() {
	case reflect.Pointer:
		v := reflect.ValueOf(implementation)
		if v.IsNil() {
			b.err = fmt.Errorf("value pointer is nil")
			return b
		}
		b.value = v
		b.valueType = t.Elem()
	case reflect.Func:
		b.function = implementation
	default:
		b.err = fmt.Errorf("implementation has type %v, want non-nil pointer or function", t)
	}
	return b
}

type continuation struct {
	RootCase    string    `json:"rootCase"`
	PC          int       `json:"programCounter"`
	AvailableAt time.Time `json:"availableAt"`
	Version     string    `json:"runtimeVersion"`
}
type Clock func() time.Time

// Scope contains only mutable execution state. Its zero value is ready to use.
type Scope struct {
	mu             sync.Mutex
	ready          map[string]bool
	continuations  map[string]continuation
	clock          Clock
	runtimeVersion string
}

func (s *Scope) init() {
	if s.ready == nil {
		s.ready = map[string]bool{}
	}
	if s.continuations == nil {
		s.continuations = map[string]continuation{}
	}
	if s.clock == nil {
		s.clock = Clock(time.Now)
	}
}
func (s *Scope) Clock() Clock { s.mu.Lock(); defer s.mu.Unlock(); s.init(); return s.clock }
func (s *Scope) SetClock(c Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if c == nil {
		s.clock = Clock(time.Now)
	} else {
		s.clock = c
	}
}

// Pending reports the deadline of a suspended root case. Applications use it
// to persist and schedule a continuation without interpreting Scope JSON.
func (s *Scope) Pending(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	continuation, exists := s.continuations[name]
	return continuation.AvailableAt, exists
}

type scopeJSON struct {
	RuntimeVersion string                  `json:"runtimeVersion,omitempty"`
	Readiness      map[string]bool         `json:"readiness,omitempty"`
	Continuations  map[string]continuation `json:"continuations,omitempty"`
}

func (s *Scope) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return json.Marshal(scopeJSON{s.runtimeVersion, s.ready, s.continuations})
}
func (s *Scope) UnmarshalJSON(data []byte) error {
	var v scopeJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeVersion = v.RuntimeVersion
	s.ready = v.Readiness
	s.continuations = v.Continuations
	s.init()
	return nil
}
