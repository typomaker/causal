package causal

//go:generate go run ./internal/bindgen/main.go

import (
	"fmt"
	"reflect"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

func newTypedBinding(name string, implementation any, function *boundFunction) Binding {
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
	function.contract = contract
	binding.function = function
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
	switch value := any(result).(type) {
	case bool:
		return types.Bool(value)
	case string:
		return types.String(value)
	case int64:
		return types.Int(value)
	case uint64:
		return types.Uint(value)
	case float64:
		return types.Double(value)
	case time.Duration:
		return types.Duration{Duration: value}
	case time.Time:
		return types.Timestamp{Time: value}
	}
	return types.DefaultTypeAdapter.NativeToValue(result)
}

func argumentTypeError(name string, index int) ref.Val {
	return types.NewErr("function %s: argument %d has incompatible type", name, index)
}
