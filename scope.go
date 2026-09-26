package causal

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// Binding supplies the runtime implementation of one [Symbol]. Bindings are
// normally created with [Bind] and passed to [Runtime.Do].
type Binding struct {
	name      string
	value     reflect.Value
	valueType reflect.Type
	function  any
	err       error
}

// Bind associates a value Symbol with a non-nil pointer, or a function Symbol
// with a function of its declared signature. For example:
//
//	causal.Bind("health", &character.Health)
//	causal.Bind("calculate_damage", calculateDamage)
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

// Clock returns the current time used to schedule and resume waits. A custom
// clock is useful when testing a program that contains [Wait].
type Clock func() time.Time

// Scope contains only mutable execution state. Its zero value is ready to use.
// Scope values are comparable. A copy is a snapshot: changing either value
// creates new internal state and makes the values compare unequal.
type Scope struct {
	state    *scopeState
	revision uint64
}

type scopeState struct {
	ready          map[string]bool
	continuations  map[string]continuation
	clock          Clock
	runtimeVersion string
}

var scopeMu sync.Mutex

func (*Scope) lock() { scopeMu.Lock() }

func (s *Scope) currentState() *scopeState {
	if s.state == nil {
		return &scopeState{clock: Clock(time.Now)}
	}
	return s.state
}

func (s *Scope) mutate() *scopeState {
	current := s.currentState()
	next := &scopeState{
		ready:          cloneMap(current.ready),
		continuations:  cloneMap(current.continuations),
		clock:          current.clock,
		runtimeVersion: current.runtimeVersion,
	}
	if next.ready == nil {
		next.ready = map[string]bool{}
	}
	if next.continuations == nil {
		next.continuations = map[string]continuation{}
	}
	s.state = next
	s.revision++
	return next
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	clone := make(map[K]V, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// Clock returns the scope's current clock. A zero-value Scope returns a clock
// backed by time.Now.
//
//	now := scope.Clock()()
func (s *Scope) Clock() Clock {
	s.lock()
	defer scopeMu.Unlock()
	return s.currentState().clock
}

// SetClock replaces the clock used by the scope. Passing nil restores the
// default time.Now clock. Tests can use it to advance a Wait deterministically:
//
//	now := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
//	scope.SetClock(func() time.Time { return now })
func (s *Scope) SetClock(c Clock) {
	s.lock()
	defer scopeMu.Unlock()
	state := s.mutate()
	if c == nil {
		state.clock = Clock(time.Now)
	} else {
		state.clock = c
	}
}

// Pending reports the deadline of a suspended root case. Applications use it
// to persist and schedule a continuation without interpreting Scope JSON.
//
//	if deadline, ok := scope.Pending("attack"); ok {
//		scheduler.Enqueue("attack", deadline)
//	}
func (s *Scope) Pending(name string) (time.Time, bool) {
	s.lock()
	defer scopeMu.Unlock()
	continuation, exists := s.currentState().continuations[name]
	return continuation.AvailableAt, exists
}

type scopeJSON struct {
	RuntimeVersion string                  `json:"runtimeVersion,omitempty"`
	Readiness      map[string]bool         `json:"readiness,omitempty"`
	Continuations  map[string]continuation `json:"continuations,omitempty"`
}

// MarshalJSON serializes execution metadata for later continuation. Application
// values and bindings are intentionally not included:
//
//	data, err := json.Marshal(&scope)
func (s *Scope) MarshalJSON() ([]byte, error) {
	s.lock()
	defer scopeMu.Unlock()
	state := s.currentState()
	return json.Marshal(scopeJSON{state.runtimeVersion, state.ready, state.continuations})
}

// UnmarshalJSON restores execution metadata previously produced by MarshalJSON:
//
//	var scope causal.Scope
//	err := json.Unmarshal(data, &scope)
func (s *Scope) UnmarshalJSON(data []byte) error {
	var v scopeJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	s.lock()
	defer scopeMu.Unlock()
	state := s.mutate()
	state.runtimeVersion = v.RuntimeVersion
	state.ready = v.Readiness
	state.continuations = v.Continuations
	if state.ready == nil {
		state.ready = map[string]bool{}
	}
	if state.continuations == nil {
		state.continuations = map[string]continuation{}
	}
	return nil
}
