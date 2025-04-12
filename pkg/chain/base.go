package chain

import (
	"fmt"
	"reflect"
)

/* 中间件链式组装器 */
// 用于将服务与中间件调用链绑定，实现AOP横向扩展

type (
	ChainImpl interface {
		MakeChainCtx(v ...any) ChainCtxImpl
	}
	ChainCtxImpl interface {
		Next()
		Stop()

		Name() string
		GetArg() []any
		GetResult() []any

		Set(string, any)
		Get(string) any
	}
	// CombineImpl interface{}
)

type (
	FilterHandle   func(ctx *ChainContext)
	ValuerTransfer func(ctx *ChainContext, args []reflect.Value) error

	FilterChain struct {
		before []FilterHandle
		after  []FilterHandle

		// 用于转换实际方法的入参，创建时赋值，流量进入时调用
		makeArg ValuerTransfer
		// 用于转换实际方法的响应，创建时赋值，实际逻辑执行完成后调用
		makeVal ValuerTransfer
	}

	// FilterChain是否抽象为接口？目前不。
)

func NewFilterChain() *FilterChain {
	filter := &FilterChain{}
	return filter
}

// CombineSrvChain 组合对应服务与中间件
// t 传入的是要加中间件的服务方法（需要为函数类型或者方法类型）
// 函数会将t动态更改为带着上下文ctx与前置后置逻辑的函数，然后按照完整的函数签名返回
func CombineSrvChain[T any](fil *FilterChain, t T) T {

	// 获取函数的反射值并校验
	// 构造调用ctx与构造新的代理函数
	// 返回代理函数

	fnValue := reflect.ValueOf(t)
	typ := reflect.TypeOf(t)
	if typ.Kind() != reflect.Func {
		return t
	}

	// 创建新的函数(具有相同函数签名)
	copiedFunc := reflect.MakeFunc(fnValue.Type(), func(args []reflect.Value) []reflect.Value {
		ctx := fil.MakeChainCtx(nil, nil)
		fmt.Println(fnValue.Type().String())
		ctx.MethodName = fnValue.String()
		if fil.makeArg != nil {
			fil.makeArg(ctx, args)
		}

		ctx.chain = append(ctx.chain, fil.before...)
		ctx.chain = append(ctx.chain, func(ctx *ChainContext) {
			res := fnValue.Call(args)
			if fil.makeVal != nil {
				fil.makeVal(ctx, res)
			}
			ctx.callResult = res
			ctx.Next()
		})
		ctx.chain = append(ctx.chain, fil.after...)

		ctx.Next()
		return ctx.callResult
	})

	return copiedFunc.Interface().(T)
}

func (ch *FilterChain) BeforeInvoke(h ...FilterHandle) {
	ch.before = h
}

func (ch *FilterChain) AfterInvoke(h ...FilterHandle) {
	ch.after = h
}

func (ch *FilterChain) MakeChainCtx(v ...any) *ChainContext {
	ctx := NewChainCtx()
	ctx.chain = make([]FilterHandle, 0, len(ch.before)+len(ch.after)+1)
	return ctx
}

func (ch *FilterChain) SetTansferFn(arg ValuerTransfer, res ValuerTransfer) {
	ch.makeArg = arg
	ch.makeVal = res
}
