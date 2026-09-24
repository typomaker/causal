package causal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/common/types/ref"
)

type stateOption interface{ apply(*stateDef) error }
type BindingOption interface{ apply(*stateDef) error }
type Binding interface{ stateBinding() }
type stateOptionFunc func(*stateDef) error

func (f stateOptionFunc) apply(s *stateDef) error { return f(s) }

type stateDef struct {
	name, kind string
	get        func(context.Context) any
	validate   func(ref.Val) error
	set        func(context.Context, ref.Val) error
}

func (stateDef) stateBinding() {}

// Getter binds func(context.Context) T using a statically typed adapter.
func Getter[T any](fn func(context.Context) T) BindingOption {
	return stateOptionFunc(func(s *stateDef) error {
		k := staticKind[T]()
		if s.kind != "" && s.kind != k {
			return fmt.Errorf("getter/setter value types differ")
		}
		s.kind = k
		s.get = func(ctx context.Context) any { return fn(ctx) }
		return nil
	})
}

// Setter binds func(context.Context, T).
func Setter[T any](fn func(context.Context, T)) BindingOption {
	return stateOptionFunc(func(s *stateDef) error {
		k := staticKind[T]()
		if s.kind != "" && s.kind != k {
			return fmt.Errorf("getter/setter value types differ")
		}
		s.kind = k
		s.validate = func(v ref.Val) error {
			if _, ok := v.Value().(T); !ok {
				return fmt.Errorf("cannot assign CEL %s to %s State", v.Type(), k)
			}
			return nil
		}
		s.set = func(ctx context.Context, v ref.Val) error {
			x := v.Value().(T) // commit preflights every pending value first
			fn(ctx, x)
			return nil
		}
		return nil
	})
}

func staticKind[T any]() string {
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
	default:
		return fmt.Sprintf("dynamic:%T", z)
	}
}

// State defines a named runtime binding. Binding validation occurs in Engine.Do.
func State(name string, opts ...BindingOption) Binding {
	s := stateDef{name: name}
	for _, opt := range opts {
		if err := opt.apply(&s); err != nil && s.name != "" {
			s.name = "\x00" + err.Error()
		}
	}
	return s
}

type continuation struct {
	RootCase    string    `json:"rootCase"`
	PC          int       `json:"programCounter"`
	AvailableAt time.Time `json:"availableAt"`
	Version     string    `json:"engineVersion"`
}

// Clock is the time source used by Wait.
type Clock func() time.Time

// Runtime owns bindings, readiness and suspended executions for one instance.
type Runtime struct {
	mu            sync.Mutex
	states        map[string]stateDef
	ready         map[string]bool
	continuations map[string]continuation
	err           error
	clock         Clock
	engineVersion string
}

// Scope creates an isolated runtime environment.
func Scope(bindings ...Binding) *Runtime {
	s := &Runtime{states: map[string]stateDef{}, ready: map[string]bool{}, continuations: map[string]continuation{}, clock: Clock(time.Now)}
	for _, binding := range bindings {
		st, ok := binding.(stateDef)
		if !ok {
			s.err = fmt.Errorf("causal: unsupported Binding %T", binding)
			continue
		}
		if len(st.name) > 0 && st.name[0] == 0 {
			s.err = fmt.Errorf("causal: invalid State: %s", st.name[1:])
			continue
		}
		if st.name == "" {
			s.err = fmt.Errorf("causal: state name is empty")
			continue
		}
		if _, ok := s.states[st.name]; ok {
			s.err = fmt.Errorf("causal: duplicate state %q", st.name)
			continue
		}
		if st.get == nil {
			s.err = fmt.Errorf("causal: state %q has no Getter", st.name)
			continue
		}
		s.states[st.name] = st
	}
	return s
}

// Clock returns the Scope time source.
func (s *Runtime) Clock() Clock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clock
}

// SetClock replaces the Scope time source. Nil restores time.Now.
func (s *Runtime) SetClock(c Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil {
		s.clock = Clock(time.Now)
	} else {
		s.clock = c
	}
}

func callGetter(ctx context.Context, st stateDef) any { return st.get(ctx) }

type scopeJSON struct {
	EngineVersion string                  `json:"engineVersion,omitempty"`
	Readiness     map[string]bool         `json:"readiness,omitempty"`
	Continuations map[string]continuation `json:"continuations,omitempty"`
}

func (s *Runtime) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(scopeJSON{s.engineVersion, s.ready, s.continuations})
}

// UnmarshalJSON restores runtime data while retaining State bindings already installed by Scope.
func (s *Runtime) UnmarshalJSON(data []byte) error {
	var v scopeJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]stateDef{}
	}
	if s.clock == nil {
		s.clock = Clock(time.Now)
	}
	s.engineVersion = v.EngineVersion
	s.ready = v.Readiness
	if s.ready == nil {
		s.ready = map[string]bool{}
	}
	s.continuations = v.Continuations
	if s.continuations == nil {
		s.continuations = map[string]continuation{}
	}
	return nil
}
