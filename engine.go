package causal

import (
	"context"
	"fmt"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter"
	"reflect"
	"time"
)

type segment struct {
	values     []segmentValue
	order      []int
	activation expressionActivation
}

type segmentValue struct {
	snapshot    any
	pending     ref.Val
	native      any
	hasSnapshot bool
	hasPending  bool
}

type preparedBinding struct {
	Binding
	function *boundFunction
	toNative func(ref.Val) (any, error)
	set      bool
}

type expressionActivation struct {
	runtime  *Runtime
	compiled *compiledCase
	context  context.Context
	bindings []preparedBinding
	segment  *segment
}

// Do validates all bindings required by the complete root Case, then executes
// until completion or the next Wait.
//
//	err := runtime.Do(ctx, &scope, "attack",
//		causal.Bind("health", &health),
//		causal.Bind("damage", &damage),
//	)
func (r *Runtime) Do(ctx context.Context, scope *Scope, name string, bindings ...Binding) error {
	if scope == nil {
		return fmt.Errorf("causal: nil scope")
	}
	execution, err := r.Prepare(name, bindings...)
	if err != nil {
		return err
	}
	return execution.Do(ctx, scope)
}

// Execution is a validated binding set for one root Case. It can be reused to
// avoid validating the same bindings on every call to [Runtime.Do].
type Execution struct {
	runtime  *Runtime
	caseName string
	compiled *compiledCase
	bindings []preparedBinding
}

// Prepare validates bindings once and returns a reusable execution.
func (r *Runtime) Prepare(name string, bindings ...Binding) (*Execution, error) {
	cc, ok := r.cases[name]
	if !ok {
		return nil, fmt.Errorf("causal: unknown case %q", name)
	}
	bound, err := r.validateBindings(cc, bindings)
	if err != nil {
		return nil, fmt.Errorf("causal: case %q: %w", name, err)
	}
	return &Execution{runtime: r, caseName: name, compiled: cc, bindings: bound}, nil
}

// Do executes the prepared root Case until completion or the next Wait.
func (e *Execution) Do(ctx context.Context, scope *Scope) error {
	if e == nil || e.runtime == nil || e.compiled == nil {
		return fmt.Errorf("causal: invalid execution")
	}
	if scope == nil {
		return fmt.Errorf("causal: nil scope")
	}
	r, name, cc, bound := e.runtime, e.caseName, e.compiled, e.bindings
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.runtimeVersion != "" && scope.runtimeVersion != r.version {
		return fmt.Errorf("causal: scope runtime version %q is incompatible with %q", scope.runtimeVersion, r.version)
	}
	scope.runtimeVersion = r.version
	pc := 0
	if c, ok := scope.continuations[name]; ok {
		if c.Version != r.version || c.RootCase != name || c.PC < 0 || c.PC >= len(cc.code) {
			return fmt.Errorf("causal: invalid continuation for %q", name)
		}
		if scope.clock().Before(c.AvailableAt) {
			return nil
		}
		pc = c.PC
	}
	scope.ready[name] = false
	seg := r.segmentPool.Get().(*segment)
	seg.activation = expressionActivation{runtime: r, compiled: cc, bindings: bound, segment: seg}
	defer r.releaseSegment(seg)
	for pc < len(cc.code) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ins := cc.code[pc]
		switch ins.kind {
		case instWith:
			v, e := r.eval(ctx, bound, seg, ins)
			if e != nil {
				return fmt.Errorf("causal: case %q With %q: %w", name, ins.source, e)
			}
			entry := &seg.values[ins.targetIndex]
			if !entry.hasPending {
				seg.order = append(seg.order, ins.targetIndex)
			}
			entry.pending = v
			entry.hasPending = true
		case instSkip:
			v, e := r.eval(ctx, bound, seg, ins)
			if e != nil {
				return fmt.Errorf("causal: case %q Skip %q: %w", name, ins.source, e)
			}
			b, ok := v.(types.Bool)
			if !ok {
				return fmt.Errorf("causal: Skip %q returned %s, want bool", ins.source, v.Type())
			}
			if bool(b) {
				pc = ins.jump
				continue
			}
		case instWait:
			v, e := r.eval(ctx, bound, seg, ins)
			if e != nil {
				return fmt.Errorf("causal: case %q Wait %q: %w", name, ins.source, e)
			}
			var availableAt time.Time
			switch wait := v.(type) {
			case types.Duration:
				if wait.Duration < 0 {
					return fmt.Errorf("causal: Wait %q returned negative duration", ins.source)
				}
				availableAt = scope.clock().Add(wait.Duration)
			case types.Timestamp:
				availableAt = wait.Time
			default:
				return fmt.Errorf("causal: Wait %q returned %s, want duration or timestamp", ins.source, v.Type())
			}
			if e := r.commit(ctx, scope, bound, seg); e != nil {
				return e
			}
			scope.continuations[name] = continuation{RootCase: name, PC: pc + 1, AvailableAt: availableAt, Version: r.version}
			return nil
		}
		pc++
	}
	if err := r.commit(ctx, scope, bound, seg); err != nil {
		return err
	}
	delete(scope.continuations, name)
	return nil
}

func (r *Runtime) validateBindings(cc *compiledCase, items []Binding) ([]preparedBinding, error) {
	prepared := make([]preparedBinding, len(cc.requirementIndexes))
	for itemIndex, b := range items {
		if b.err != nil {
			return nil, fmt.Errorf("invalid binding %q: %w", b.name, b.err)
		}
		if b.name == "" {
			return nil, fmt.Errorf("binding name is empty")
		}
		index, ok := r.symbolIndexes[b.name]
		if !ok {
			return nil, fmt.Errorf("binding for undeclared symbol %q", b.name)
		}
		for previous := range itemIndex {
			if items[previous].name == b.name {
				return nil, fmt.Errorf("duplicate binding %q", b.name)
			}
		}
		c := r.symbols[index]
		slot, required := cc.bindingSlots[index]
		if c.function {
			if b.function == nil || b.value.IsValid() {
				return nil, fmt.Errorf("function symbol %q binding must be a function", b.name)
			}
			if err := validateFunctionBinding(c, b.function.contract); err != nil {
				return nil, err
			}
			if required {
				prepared[slot] = preparedBinding{Binding: b, function: b.function, set: true}
			}
		} else {
			if !b.value.IsValid() || b.function != nil {
				return nil, fmt.Errorf("value symbol %q binding must be a non-nil pointer", b.name)
			}
			kind, supported := kindForType(b.valueType)
			if !supported || kind != c.kind || c.goType != nil && b.valueType != c.goType {
				return nil, fmt.Errorf("symbol %q binding type %v does not match %s", b.name, b.valueType, c.kind)
			}
			converter := c.toNative
			if converter == nil {
				converter = nativeConverter(b.valueType)
			}
			if required {
				prepared[slot] = preparedBinding{Binding: b, toNative: converter, set: true}
			}
		}
	}
	for slot, index := range cc.requirementIndexes {
		if !prepared[slot].set {
			return nil, fmt.Errorf("missing binding for symbol %q", r.symbols[index].symbol.Name)
		}
	}
	return prepared, nil
}

func (r *Runtime) eval(ctx context.Context, bound []preparedBinding, seg *segment, ins instruction) (ref.Val, error) {
	for _, index := range ins.expr.readIndexes {
		entry := &seg.values[index]
		if !entry.hasPending && !entry.hasSnapshot {
			entry.snapshot = bound[seg.activation.compiled.bindingSlots[index]].value.Elem().Interface()
			entry.hasSnapshot = true
		}
	}
	seg.activation.context = ctx
	v, _, e := ins.expr.program.Eval(&seg.activation)
	if e != nil {
		return nil, e
	}
	if types.IsError(v) {
		return nil, fmt.Errorf("%v", v)
	}
	return v, nil
}
func (a *expressionActivation) ResolveName(name string) (any, bool) {
	if name == executionActivationName {
		return a, true
	}
	index, ok := a.runtime.symbolIndexes[name]
	if !ok {
		return nil, false
	}
	slot, ok := a.compiled.bindingSlots[index]
	if !ok || !a.bindings[slot].set {
		return nil, false
	}
	entry := &a.segment.values[index]
	if entry.hasPending {
		return entry.pending, true
	}
	if entry.hasSnapshot {
		return entry.snapshot, true
	}
	return nil, false
}

func (*expressionActivation) Parent() interpreter.Activation { return nil }

func (r *Runtime) commit(ctx context.Context, s *Scope, bound []preparedBinding, seg *segment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, index := range seg.order {
		entry := &seg.values[index]
		binding := bound[seg.activation.compiled.bindingSlots[index]]
		native, err := binding.toNative(entry.pending)
		if err != nil {
			return fmt.Errorf("causal: symbol %q: cannot assign CEL %s to %v Symbol", r.symbols[index].symbol.Name, entry.pending.Type(), binding.valueType)
		}
		entry.native = native
	}
	for _, index := range seg.order {
		bound[seg.activation.compiled.bindingSlots[index]].value.Elem().Set(reflect.ValueOf(seg.values[index].native))
	}
	for _, index := range seg.order {
		for _, c := range r.dependents[index] {
			s.ready[c] = true
		}
	}
	resetSegment(seg)
	return nil
}

func (r *Runtime) releaseSegment(seg *segment) {
	resetSegment(seg)
	seg.activation = expressionActivation{}
	r.segmentPool.Put(seg)
}

func resetSegment(seg *segment) {
	clear(seg.values)
	seg.order = seg.order[:0]
}

type functionCall func(context.Context, []ref.Val) ref.Val
type functionCall0 func(context.Context) ref.Val
type functionCall1 func(context.Context, ref.Val) ref.Val
type functionCall2 func(context.Context, ref.Val, ref.Val) ref.Val
type functionCall3 func(context.Context, ref.Val, ref.Val, ref.Val) ref.Val
type functionCall4 func(context.Context, ref.Val, ref.Val, ref.Val, ref.Val) ref.Val
type functionCall5 func(context.Context, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val) ref.Val
type functionCall6 func(context.Context, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val) ref.Val
type functionCall7 func(context.Context, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val) ref.Val
type functionCall8 func(context.Context, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val, ref.Val) ref.Val

type boundFunction struct {
	contract   symbolContract
	call       functionCall
	call0      functionCall0
	call1      functionCall1
	call2      functionCall2
	call3      functionCall3
	call4      functionCall4
	call5      functionCall5
	call6      functionCall6
	call7      functionCall7
	call8      functionCall8
	typedArity int
}

func prepareFunction(name string, fn any) (*boundFunction, error) {
	t := reflect.TypeOf(fn)
	if t == nil {
		return nil, fmt.Errorf("function symbol %q implementation has type %v", name, t)
	}
	c, err := contractForType(name, t)
	if err != nil {
		return nil, err
	}
	f := reflect.ValueOf(fn)
	if f.IsNil() {
		return nil, fmt.Errorf("function symbol %q implementation is nil", name)
	}
	call := func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != len(c.args) {
			return types.NewErr("function %s: got %d arguments, want %d", c.symbol.Name, len(args), len(c.args))
		}
		in := make([]reflect.Value, 0, t.NumIn())
		if c.context {
			in = append(in, reflect.ValueOf(ctx))
		}
		for i, arg := range args {
			target := c.nativeArgs[i]
			value, err := nativeValue(arg, target)
			if err != nil {
				return types.NewErr("function %s: argument %d has incompatible type", c.symbol.Name, i)
			}
			in = append(in, reflect.ValueOf(value))
		}
		out := f.Call(in)
		if c.returnsError && !out[1].IsNil() {
			return types.NewErr("%s", out[1].Interface().(error))
		}
		return types.DefaultTypeAdapter.NativeToValue(out[0].Interface())
	}
	return &boundFunction{contract: c, call: call, typedArity: -1}, nil
}

func validateFunctionBinding(expected, actual symbolContract) error {
	if expected.goType != nil {
		if actual.goType != expected.goType {
			return fmt.Errorf("function symbol %q implementation has type %v, want %v", expected.symbol.Name, actual.goType, expected.goType)
		}
		return nil
	}
	if !actual.function || actual.result != expected.result || !sameKinds(actual.args, expected.args) {
		return fmt.Errorf("function symbol %q implementation has incompatible CEL signature", expected.symbol.Name)
	}
	return nil
}

func functionAdapter(c symbolContract, fn any) (functionCall, error) {
	bound, err := prepareFunction(c.symbol.Name, fn)
	if err != nil {
		return nil, err
	}
	if err := validateFunctionBinding(c, bound.contract); err != nil {
		return nil, err
	}
	return bound.call, nil
}

func nativeValue(value ref.Val, target reflect.Type) (any, error) {
	switch target {
	case reflect.TypeFor[bool]():
		if v, ok := value.(types.Bool); ok {
			return bool(v), nil
		}
	case reflect.TypeFor[string]():
		if v, ok := value.(types.String); ok {
			return string(v), nil
		}
	case reflect.TypeFor[int64]():
		if v, ok := value.(types.Int); ok {
			return int64(v), nil
		}
	case reflect.TypeFor[uint64]():
		if v, ok := value.(types.Uint); ok {
			return uint64(v), nil
		}
	case reflect.TypeFor[float64]():
		if v, ok := value.(types.Double); ok {
			return float64(v), nil
		}
	case durationType:
		if v, ok := value.(types.Duration); ok {
			return v.Duration, nil
		}
	}
	if target.Kind() != reflect.Array {
		return value.ConvertToNative(target)
	}
	list, ok := value.(traits.Lister)
	if !ok || int(list.Size().(types.Int)) != target.Len() {
		return nil, fmt.Errorf("list length does not match %v", target)
	}
	result := reflect.New(target).Elem()
	for i := 0; i < target.Len(); i++ {
		element, err := nativeValue(list.Get(types.Int(i)), target.Elem())
		if err != nil {
			return nil, err
		}
		result.Index(i).Set(reflect.ValueOf(element))
	}
	return result.Interface(), nil
}

func nativeConverter(target reflect.Type) func(ref.Val) (any, error) {
	return func(value ref.Val) (any, error) { return nativeValue(value, target) }
}

func sameKinds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
