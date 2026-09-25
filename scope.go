package causal

import (
	"encoding/json"
	"fmt"
	"github.com/google/cel-go/common/types/ref"
	"sync"
	"time"
)

type bindingOption interface{ apply(*Binding) error }
type bindingOptionFunc func(*Binding) error

func (f bindingOptionFunc) apply(b *Binding) error { return f(b) }

// Binding supplies the runtime implementation of one Symbol.
type Binding struct {
	name, kind string
	get        func() any
	validate   func(ref.Val) error
	set        func(ref.Val)
	function   any
	err        error
}

func Bind(name string, implementations ...any) Binding {
	b := Binding{name: name}
	for _, raw := range implementations {
		if o, ok := raw.(bindingOption); ok {
			if e := o.apply(&b); e != nil {
				b.err = e
			}
		} else if b.function == nil {
			b.function = raw
		} else {
			b.err = fmt.Errorf("multiple function implementations")
		}
	}
	return b
}
func Getter[T any](fn func() T) bindingOption {
	return bindingOptionFunc(func(b *Binding) error {
		k := valueKind[T]()
		if b.kind != "" && b.kind != k {
			return fmt.Errorf("getter/setter value types differ")
		}
		b.kind = k
		b.get = func() any { return fn() }
		return nil
	})
}
func Setter[T any](fn func(T)) bindingOption {
	return bindingOptionFunc(func(b *Binding) error {
		k := valueKind[T]()
		if b.kind != "" && b.kind != k {
			return fmt.Errorf("getter/setter value types differ")
		}
		b.kind = k
		b.validate = func(v ref.Val) error {
			if _, ok := v.Value().(T); !ok {
				return fmt.Errorf("cannot assign CEL %s to %s Symbol", v.Type(), k)
			}
			return nil
		}
		b.set = func(v ref.Val) { fn(v.Value().(T)) }
		return nil
	})
}
func valueKind[T any]() string {
	var z T
	switch any(z).(type) {
	case bool:
		return "bool"
	case string:
		return "string"
	case int64:
		return "int"
	case uint64:
		return "uint"
	case float64:
		return "double"
	case time.Duration:
		return "duration"
	}
	return fmt.Sprintf("unsupported:%T", z)
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
