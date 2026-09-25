package causal

import (
	"context"
	"fmt"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"reflect"
)

type segment struct {
	snapshot, pending map[string]any
	order             []string
}

// Do validates all bindings required by the complete root Case, then executes
// until completion or the next Wait.
func (r *Runtime) Do(ctx context.Context, scope *Scope, name string, bindings ...Binding) error {
	if scope == nil {
		return fmt.Errorf("causal: nil scope")
	}
	cc, ok := r.cases[name]
	if !ok {
		return fmt.Errorf("causal: unknown case %q", name)
	}
	bound, err := r.validateBindings(cc, bindings)
	if err != nil {
		return fmt.Errorf("causal: case %q: %w", name, err)
	}
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
	seg := &segment{snapshot: map[string]any{}, pending: map[string]any{}}
	for pc < len(cc.code) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ins := cc.code[pc]
		switch ins.kind {
		case instWith:
			b := bound[ins.target]
			v, e := r.eval(ctx, bound, seg, ins)
			if e != nil {
				return fmt.Errorf("causal: case %q With %q: %w", name, ins.source, e)
			}
			if _, ok := seg.pending[ins.target]; !ok {
				seg.order = append(seg.order, ins.target)
			}
			seg.pending[ins.target] = v
			_ = b
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
			d, ok := v.(types.Duration)
			if !ok {
				return fmt.Errorf("causal: Wait %q returned %s, want duration", ins.source, v.Type())
			}
			if d.Duration < 0 {
				return fmt.Errorf("causal: Wait %q returned negative duration", ins.source)
			}
			if e := r.commit(ctx, scope, bound, seg); e != nil {
				return e
			}
			scope.continuations[name] = continuation{RootCase: name, PC: pc + 1, AvailableAt: scope.clock().Add(d.Duration), Version: r.version}
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

func (r *Runtime) validateBindings(cc *compiledCase, items []Binding) (map[string]Binding, error) {
	m := map[string]Binding{}
	for _, b := range items {
		if b.err != nil {
			return nil, fmt.Errorf("invalid binding %q: %w", b.name, b.err)
		}
		if b.name == "" {
			return nil, fmt.Errorf("binding name is empty")
		}
		if _, ok := m[b.name]; ok {
			return nil, fmt.Errorf("duplicate binding %q", b.name)
		}
		c, ok := r.contracts[b.name]
		if !ok {
			return nil, fmt.Errorf("binding for undeclared symbol %q", b.name)
		}
		if c.function {
			if b.function == nil {
				return nil, fmt.Errorf("function symbol %q has no implementation", b.name)
			}
			if _, err := functionAdapter(c, b.function); err != nil {
				return nil, err
			}
		} else {
			if b.get == nil {
				return nil, fmt.Errorf("symbol %q has no Getter", b.name)
			}
			if b.kind != c.kind {
				return nil, fmt.Errorf("symbol %q binding type %s does not match %s", b.name, b.kind, c.kind)
			}
		}
		m[b.name] = b
	}
	for n, q := range cc.requirements {
		b, ok := m[n]
		if !ok {
			return nil, fmt.Errorf("missing binding for symbol %q", n)
		}
		if q.write && b.set == nil {
			return nil, fmt.Errorf("symbol %q is read-only", n)
		}
	}
	return m, nil
}

func (r *Runtime) eval(ctx context.Context, bound map[string]Binding, seg *segment, ins instruction) (ref.Val, error) {
	activation := map[string]any{}
	ids := map[string]struct{}{}
	if err := addDeps(ins.ast, ids); err != nil {
		return nil, err
	}
	for dep := range ids {
		c, ok := r.contracts[dep]
		if !ok || c.function {
			continue
		}
		if v, ok := seg.pending[dep]; ok {
			activation[dep] = v
			continue
		}
		if v, ok := seg.snapshot[dep]; ok {
			activation[dep] = v
			continue
		}
		v := bound[dep].get(ctx)
		seg.snapshot[dep] = v
		activation[dep] = v
	}
	opts := []cel.EnvOption{}
	for n, c := range r.contracts {
		if !c.function {
			continue
		}
		b, ok := bound[n]
		if !ok {
			continue
		}
		call, _ := functionAdapter(c, b.function)
		args := make([]*cel.Type, len(c.args))
		for i, kind := range c.args {
			args[i] = celType(kind)
		}
		opts = append(opts, cel.Function(n, cel.Overload(overloadID(n), args, celType(c.result),
			cel.FunctionBinding(func(args ...ref.Val) ref.Val { return call(ctx, args) }))))
	}
	evalEnv, e := r.env.Extend(opts...)
	if e != nil {
		return nil, e
	}
	p, e := evalEnv.Program(ins.ast)
	if e != nil {
		return nil, e
	}
	v, _, e := p.Eval(activation)
	if e != nil {
		return nil, e
	}
	if types.IsError(v) {
		return nil, fmt.Errorf("%v", v)
	}
	return v, nil
}
func (r *Runtime) commit(ctx context.Context, s *Scope, bound map[string]Binding, seg *segment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, n := range seg.order {
		v := seg.pending[n].(ref.Val)
		if err := bound[n].validate(v); err != nil {
			return fmt.Errorf("causal: symbol %q: %w", n, err)
		}
	}
	for _, n := range seg.order {
		bound[n].set(ctx, seg.pending[n].(ref.Val))
	}
	for _, n := range seg.order {
		for _, c := range r.dependents[n] {
			s.ready[c] = true
		}
	}
	seg.snapshot = map[string]any{}
	seg.pending = map[string]any{}
	seg.order = nil
	return nil
}

type functionCall func(context.Context, []ref.Val) ref.Val

func functionAdapter(c symbolContract, fn any) (functionCall, error) {
	t := reflect.TypeOf(fn)
	if t == nil {
		return nil, fmt.Errorf("function symbol %q implementation has type %v, want %v", c.symbol.Name, t, c.goType)
	}
	if c.goType != nil {
		if t != c.goType {
			return nil, fmt.Errorf("function symbol %q implementation has type %v, want %v", c.symbol.Name, t, c.goType)
		}
	} else {
		actual, err := contractForType(c.symbol.Name, t)
		if err != nil || !actual.function || actual.result != c.result || !sameKinds(actual.args, c.args) {
			return nil, fmt.Errorf("function symbol %q implementation has incompatible CEL signature", c.symbol.Name)
		}
		c.context, c.returnsError = actual.context, actual.returnsError
	}
	f := reflect.ValueOf(fn)
	if f.IsNil() {
		return nil, fmt.Errorf("function symbol %q implementation is nil", c.symbol.Name)
	}
	return func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != len(c.args) {
			return types.NewErr("function %s: got %d arguments, want %d", c.symbol.Name, len(args), len(c.args))
		}
		in := make([]reflect.Value, 0, t.NumIn())
		if c.context {
			in = append(in, reflect.ValueOf(ctx))
		}
		for i, arg := range args {
			value := reflect.ValueOf(arg.Value())
			target := t.In(i)
			if c.context {
				target = t.In(i + 1)
			}
			if !value.IsValid() || !value.Type().ConvertibleTo(target) {
				return types.NewErr("function %s: argument %d has incompatible type", c.symbol.Name, i)
			}
			in = append(in, value.Convert(target))
		}
		out := f.Call(in)
		if c.returnsError && !out[1].IsNil() {
			return types.NewErr("%s", out[1].Interface().(error))
		}
		return types.DefaultTypeAdapter.NativeToValue(out[0].Interface())
	}, nil
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
