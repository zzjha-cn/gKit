package chain

import "reflect"

type (
	// 整体抽象
	ChainImpl interface {
		// 构造上下文，上下文中已经包含了链式逻辑的构造，可以直接传入参数调用链式逻辑
		MakeChainCtx(bd *CtxBuilder) ContextImpl
	}

	// 上下文抽象
	ContextImpl interface {
		Next() // 下一个中间件
		Stop() // 终止

		Name() string
		GetArg() []any    // 获取上下文中存储着的正式函数入参（因为有的中间件是需要判断入参的）
		GetResult() []any // 获取上下文中存储的正式方法的响应内容（注意只有after的中间件才拿得到内容）

		Set(string, any) // 设置会话的上下文缓存数据
		Get(string) any  // 获取会话的上下文缓存数据

		GetCtxBuild() *CtxBuilder
	}

	CtxBuilder struct {
		Name         string                                     // 上下文传入的方法名称
		Args         []reflect.Value                            // 需要调用实际逻辑的入参
		callResult   []reflect.Value                            // 实际逻辑的返回内容
		userFunction func(args []reflect.Value) []reflect.Value // 通过反射拿到的实际逻辑调用
	}
)

func NewBuilder() *CtxBuilder {
	return &CtxBuilder{}
}
func (b *CtxBuilder) SetCallResult(x []reflect.Value) {
	b.callResult = x
}
func (b *CtxBuilder) GetCallResult() []reflect.Value {
	return b.callResult
}

// 执行实际逻辑
func (b *CtxBuilder) Handle() []reflect.Value {
	res := b.userFunction(b.Args)
	b.callResult = res
	return res
}

// ================================================================

// WrapChain 动态代理包装方法
// t 传入的是要加中间件的服务方法（需要为函数类型或者方法类型）
// 函数会将t动态更改为带着上下文ctx与前置后置逻辑的函数，然后按照完整的函数签名返回
func WrapChainAsFunc[T any](ch ChainImpl, srvMethod T) T {

	// 获取函数的反射值并校验
	// 构造调用ctx与构造新的代理函数
	// 返回代理函数

	methodTyp := reflect.TypeOf(srvMethod)
	if methodTyp.Kind() != reflect.Func {
		return srvMethod
	}

	methodVal := reflect.ValueOf(srvMethod)

	// 创建新的函数(具有相同函数签名)
	copiedFunc := reflect.MakeFunc(methodTyp, func(args []reflect.Value) (results []reflect.Value) {
		bd := NewBuilder()
		bd.Name = methodVal.String()
		bd.Args = args
		bd.userFunction = func(args []reflect.Value) []reflect.Value {
			return methodVal.Call(args)
		}

		// 构造上下文
		// 上下文中会结合builder构造出调用链（before - handler - after）
		ctx := ch.MakeChainCtx(bd)

		// 执行调用链
		ctx.Next()

		return bd.GetCallResult()
	})

	return copiedFunc.Interface().(T)
}
