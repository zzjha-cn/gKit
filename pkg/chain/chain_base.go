package chain

import "reflect"

type (
	// ChainInterface interface {
	// 	MakeChainCtx(v any) *ChainContext
	// 	Next(ctx *ChainContext, ind int)
	// }

	FilterHandle func(ctx *ChainContext)

	ChainContext struct {
		MethodName string
		Arg        []any
		Val        []any
		chain      []FilterHandle
	}

	FilterChain struct {
		chain  []FilterHandle
		before []int
		after  []int

		makeArg func(ctx *ChainContext, args []reflect.Value) error
		makeVal func(ctx *ChainContext, args []reflect.Value) error
	}
)

// CombineSrvChain 组合对应服务与中间件
// t 传入的是要加中间件的服务方法（需要为函数类型或者方法类型）
// 函数会将t动态更改为带着上下文ctx与前置后置逻辑的函数，然后按照完整的函数签名返回
func CombineSrvChain[T any](fil *FilterChain, t T) T {

	// 获取函数的反射值并校验
	// 构造调用ctx与构造新的代理函数
	// 返回新的代理函数

	fnValue := reflect.ValueOf(t)
	typ := reflect.TypeOf(t)
	if typ.Kind() != reflect.Func {
		return t
	}

	// 创建一个新的函数
	copiedFunc := reflect.MakeFunc(fnValue.Type(), func(args []reflect.Value) []reflect.Value {
		if fil.makeArg != nil {
			fil.makeArg(nil, args)
		}

		if fil.makeVal != nil {
			fil.makeVal(nil, args)
		}

		return fnValue.Call(args)
	})

	return copiedFunc.Interface().(T)
}
