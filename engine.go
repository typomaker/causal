package causal

import (
	"context"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter/functions"
)

type segment struct {
	snapshot, pending map[string]any
	order             []string
}

// Do executes one explicitly named Case until END or the next Wait.
func (e *Engine) Do(ctx context.Context, scope *Runtime, name string) error {
	if scope == nil {
		return fmt.Errorf("causal: nil scope")
	}
	cc, ok := e.cases[name]
	if !ok {
		return fmt.Errorf("causal: unknown case %q", name)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.err != nil {
		return scope.err
	}
	if scope.engineVersion != "" && scope.engineVersion != e.version {
		return fmt.Errorf("causal: scope engine version %q is incompatible with %q", scope.engineVersion, e.version)
	}
	if scope.engineVersion == "" {
		scope.engineVersion = e.version
	}
	pc := 0
	if c, ok := scope.continuations[name]; ok {
		if c.Version != e.version {
			return fmt.Errorf("causal: continuation for %q has incompatible engine version %q", name, c.Version)
		}
		if c.RootCase != name || c.PC < 0 || c.PC >= len(cc.code) {
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
			st, ok := scope.states[ins.target]
			if !ok {
				return fmt.Errorf("causal: case %q: unknown target state %q", name, ins.target)
			}
			if st.set == nil {
				return fmt.Errorf("causal: case %q: state %q is read-only", name, ins.target)
			}
			v, err := e.eval(ctx, scope, seg, ins)
			if err != nil {
				return fmt.Errorf("causal: case %q With %q: %w", name, ins.source, err)
			}
			if _, exists := seg.pending[ins.target]; !exists {
				seg.order = append(seg.order, ins.target)
			}
			seg.pending[ins.target] = v
		case instSkip:
			v, err := e.eval(ctx, scope, seg, ins)
			if err != nil {
				return fmt.Errorf("causal: case %q Skip %q: %w", name, ins.source, err)
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
			v, err := e.eval(ctx, scope, seg, ins)
			if err != nil {
				return fmt.Errorf("causal: case %q Wait %q: %w", name, ins.source, err)
			}
			d, ok := v.(types.Duration)
			if !ok {
				return fmt.Errorf("causal: Wait %q returned %s, want duration", ins.source, v.Type())
			}
			if d.Duration < 0 {
				return fmt.Errorf("causal: Wait %q returned negative duration", ins.source)
			}
			if err := e.commit(ctx, scope, seg); err != nil {
				return err
			}
			scope.continuations[name] = continuation{RootCase: name, PC: pc + 1, AvailableAt: scope.clock().Add(d.Duration), Version: e.version}
			return nil
		case instEndCase:
		}
		pc++
	}
	if err := e.commit(ctx, scope, seg); err != nil {
		return err
	}
	delete(scope.continuations, name)
	return nil
}

func (e *Engine) eval(ctx context.Context, s *Runtime, seg *segment, ins instruction) (ref.Val, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	activation := map[string]any{}
	for dep := range e.casesForAST(ins.ast) {
		if v, ok := seg.pending[dep]; ok {
			activation[dep] = v
			continue
		}
		if v, ok := seg.snapshot[dep]; ok {
			activation[dep] = v
			continue
		}
		st, ok := s.states[dep]
		if !ok {
			return nil, fmt.Errorf("unknown state %q", dep)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v := callGetter(ctx, st)
		seg.snapshot[dep] = v
		activation[dep] = v
	}
	opts := []cel.ProgramOption{}
	if len(e.funcs) > 0 {
		ovs := make([]*functions.Overload, 0, len(e.funcs))
		for _, f := range e.funcs {
			fn := f
			ovs = append(ovs, &functions.Overload{Operator: overloadID(fn.name), Function: func(args ...ref.Val) ref.Val { return invokeFunc(ctx, fn, args) }})
		}
		opts = append(opts, cel.Functions(ovs...))
	}
	p, err := e.env.Program(ins.ast, opts...)
	if err != nil {
		return nil, err
	}
	v, _, err := p.Eval(activation)
	if err != nil {
		return nil, err
	}
	if types.IsError(v) {
		return nil, fmt.Errorf("%v", v)
	}
	return v, nil
}
func (e *Engine) casesForAST(ast *cel.Ast) map[string]struct{} {
	m := map[string]struct{}{}
	addDeps(ast, m)
	for n := range e.funcs {
		delete(m, n)
	}
	return m
}
func invokeFunc(ctx context.Context, f *funcDef, args []ref.Val) ref.Val {
	if err := ctx.Err(); err != nil {
		return celError(err)
	}
	return f.invoke(ctx, args)
}
func (e *Engine) commit(ctx context.Context, s *Runtime, seg *segment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(seg.pending) == 0 {
		return nil
	}
	changed := append([]string(nil), seg.order...)
	// Validate the complete write set before invoking any application setter.
	// This preserves segment atomicity for conversion errors even when the bad
	// value is not the first state in setter order.
	for _, name := range changed {
		v := seg.pending[name].(ref.Val)
		st := s.states[name]
		if st.validate != nil {
			if err := st.validate(v); err != nil {
				return fmt.Errorf("causal: state %q: %w", name, err)
			}
		}
	}
	for _, name := range changed {
		v := seg.pending[name].(ref.Val)
		st := s.states[name]
		if err := st.set(ctx, v); err != nil {
			return fmt.Errorf("causal: state %q: %w", name, err)
		}
	}
	for _, state := range changed {
		for _, c := range e.dependents[state] {
			s.ready[c] = true
		}
	}
	seg.snapshot = map[string]any{}
	seg.pending = map[string]any{}
	seg.order = nil
	return nil
}
