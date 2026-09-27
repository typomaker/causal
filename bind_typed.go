package causal

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// BindFunc0 binds a zero-argument function without using reflection when CEL invokes it.
func BindFunc0[R any](name string, function func() R) Binding {
	return bindTyped0(name, function, func(context.Context) (R, error) { return function(), nil })
}

// BindFunc0Err binds a zero-argument function that returns an error.
func BindFunc0Err[R any](name string, function func() (R, error)) Binding {
	return bindTyped0(name, function, func(context.Context) (R, error) { return function() })
}

// BindContextFunc0 binds a zero-argument function that receives the execution context.
func BindContextFunc0[R any](name string, function func(context.Context) R) Binding {
	return bindTyped0(name, function, func(ctx context.Context) (R, error) { return function(ctx), nil })
}

// BindContextFunc0Err binds a zero-argument context function that returns an error.
func BindContextFunc0Err[R any](name string, function func(context.Context) (R, error)) Binding {
	return bindTyped0(name, function, function)
}

// BindFunc1 binds a one-argument function without using reflection when CEL invokes it.
func BindFunc1[A, R any](name string, function func(A) R) Binding {
	return bindTyped1(name, function, func(_ context.Context, a A) (R, error) { return function(a), nil })
}

// BindFunc1Err binds a one-argument function that returns an error.
func BindFunc1Err[A, R any](name string, function func(A) (R, error)) Binding {
	return bindTyped1(name, function, func(_ context.Context, a A) (R, error) { return function(a) })
}

// BindContextFunc1 binds a one-argument function that receives the execution context.
func BindContextFunc1[A, R any](name string, function func(context.Context, A) R) Binding {
	return bindTyped1(name, function, func(ctx context.Context, a A) (R, error) { return function(ctx, a), nil })
}

// BindContextFunc1Err binds a one-argument context function that returns an error.
func BindContextFunc1Err[A, R any](name string, function func(context.Context, A) (R, error)) Binding {
	return bindTyped1(name, function, function)
}

// BindFunc2 binds a two-argument function without using reflection when CEL invokes it.
func BindFunc2[A, B, R any](name string, function func(A, B) R) Binding {
	return bindTyped2(name, function, func(_ context.Context, a A, b B) (R, error) { return function(a, b), nil })
}

// BindFunc2Err binds a two-argument function that returns an error.
func BindFunc2Err[A, B, R any](name string, function func(A, B) (R, error)) Binding {
	return bindTyped2(name, function, func(_ context.Context, a A, b B) (R, error) { return function(a, b) })
}

// BindContextFunc2 binds a two-argument function that receives the execution context.
func BindContextFunc2[A, B, R any](name string, function func(context.Context, A, B) R) Binding {
	return bindTyped2(name, function, func(ctx context.Context, a A, b B) (R, error) { return function(ctx, a, b), nil })
}

// BindContextFunc2Err binds a two-argument context function that returns an error.
func BindContextFunc2Err[A, B, R any](name string, function func(context.Context, A, B) (R, error)) Binding {
	return bindTyped2(name, function, function)
}

// BindFunc3 binds a three-argument function without using reflection when CEL invokes it.
func BindFunc3[A, B, C, R any](name string, function func(A, B, C) R) Binding {
	return bindTyped3(name, function, func(_ context.Context, a A, b B, c C) (R, error) { return function(a, b, c), nil })
}

// BindFunc3Err binds a three-argument function that returns an error.
func BindFunc3Err[A, B, C, R any](name string, function func(A, B, C) (R, error)) Binding {
	return bindTyped3(name, function, func(_ context.Context, a A, b B, c C) (R, error) { return function(a, b, c) })
}

// BindContextFunc3 binds a three-argument function that receives the execution context.
func BindContextFunc3[A, B, C, R any](name string, function func(context.Context, A, B, C) R) Binding {
	return bindTyped3(name, function, func(ctx context.Context, a A, b B, c C) (R, error) { return function(ctx, a, b, c), nil })
}

// BindContextFunc3Err binds a three-argument context function that returns an error.
func BindContextFunc3Err[A, B, C, R any](name string, function func(context.Context, A, B, C) (R, error)) Binding {
	return bindTyped3(name, function, function)
}

func bindTyped0[R any](name string, implementation any, invoke func(context.Context) (R, error)) Binding {
	return newTypedBinding(name, implementation, func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != 0 {
			return argumentCountError(name, len(args), 0)
		}
		result, err := invoke(ctx)
		return typedResult(result, err)
	})
}

func bindTyped1[A, R any](name string, implementation any, invoke func(context.Context, A) (R, error)) Binding {
	return newTypedBinding(name, implementation, func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != 1 {
			return argumentCountError(name, len(args), 1)
		}
		a, err := typedArgument[A](args[0])
		if err != nil {
			return argumentTypeError(name, 0)
		}
		result, err := invoke(ctx, a)
		return typedResult(result, err)
	})
}

func bindTyped2[A, B, R any](name string, implementation any, invoke func(context.Context, A, B) (R, error)) Binding {
	return newTypedBinding(name, implementation, func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != 2 {
			return argumentCountError(name, len(args), 2)
		}
		a, err := typedArgument[A](args[0])
		if err != nil {
			return argumentTypeError(name, 0)
		}
		b, err := typedArgument[B](args[1])
		if err != nil {
			return argumentTypeError(name, 1)
		}
		result, err := invoke(ctx, a, b)
		return typedResult(result, err)
	})
}

func bindTyped3[A, B, C, R any](name string, implementation any, invoke func(context.Context, A, B, C) (R, error)) Binding {
	return newTypedBinding(name, implementation, func(ctx context.Context, args []ref.Val) ref.Val {
		if len(args) != 3 {
			return argumentCountError(name, len(args), 3)
		}
		a, err := typedArgument[A](args[0])
		if err != nil {
			return argumentTypeError(name, 0)
		}
		b, err := typedArgument[B](args[1])
		if err != nil {
			return argumentTypeError(name, 1)
		}
		c, err := typedArgument[C](args[2])
		if err != nil {
			return argumentTypeError(name, 2)
		}
		result, err := invoke(ctx, a, b, c)
		return typedResult(result, err)
	})
}

func newTypedBinding(name string, implementation any, call functionCall) Binding {
	binding := Binding{name: name}
	t := reflect.TypeOf(implementation)
	if t == nil {
		binding.err = fmt.Errorf("implementation is nil")
		return binding
	}
	contract, err := contractForType(name, t)
	if err != nil {
		binding.err = err
		return binding
	}
	if reflect.ValueOf(implementation).IsNil() {
		binding.err = fmt.Errorf("function symbol %q implementation is nil", name)
		return binding
	}
	binding.function = &boundFunction{contract: contract, call: call}
	return binding
}

func typedArgument[T any](value ref.Val) (T, error) {
	var zero T
	native, err := nativeValue(value, reflect.TypeFor[T]())
	if err != nil {
		return zero, err
	}
	result, ok := native.(T)
	if !ok {
		return zero, fmt.Errorf("converted argument has type %T", native)
	}
	return result, nil
}

func typedResult[T any](result T, err error) ref.Val {
	if err != nil {
		return types.NewErr("%s", err)
	}
	return types.DefaultTypeAdapter.NativeToValue(result)
}

func argumentCountError(name string, got, want int) ref.Val {
	return types.NewErr("function %s: got %d arguments, want %d", name, got, want)
}

func argumentTypeError(name string, index int) ref.Val {
	return types.NewErr("function %s: argument %d has incompatible type", name, index)
}
