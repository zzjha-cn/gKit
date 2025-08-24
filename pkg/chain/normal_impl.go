package chain

import (
	"reflect"
)

/* 中间件链式组装器 */
// 用于将服务与中间件调用链绑定，实现AOP横向扩展
// 比如某一个对象存在多个方法，要为每一个方法各自包装before与after的逻辑
// - 抽象了整体接口，实现接口即可完成中间件组链
// - 提供了默认的实现（chain与context），详见_test

type (
	// 默认实现的可以用组装器
	NormalChain struct {
		before []Filter
		after  []Filter

		// 用于转换实际方法的入参并存入ctx中，创建时赋值，流量进入时调用
		collectArg ValuerCollector
		// 用于转换实际方法的响应并存入ctx中，创建时赋值，实际逻辑执行完成后调用
		collectFuncReturn ValuerCollector
	}

	// 中间件过滤逻辑
	Filter func(ctx *NormalCtx)
	// 将参数采集进入context中的逻辑
	ValuerCollector func(ctx *NormalCtx, args []reflect.Value) error
)

func NewNormalChain() *NormalChain {
	filter := &NormalChain{}
	return filter
}

func (ch *NormalChain) MakeChainCtx(bd *CtxBuilder) ContextImpl {
	ctx := NewNormalCtx()
	ctx.chain = make([]Filter, 0, len(ch.before)+len(ch.after)+1)
	ctx.Builder = bd
	ctx.MethodName = bd.Name
	if ch.collectArg != nil {
		ch.collectArg(ctx, bd.Args)
	}

	// before逻辑
	ctx.chain = append(ctx.chain, ch.before...)
	// 实际业务逻辑
	ctx.chain = append(ctx.chain, func(ctx *NormalCtx) {
		callRes := ctx.Builder.Handle()
		if ch.collectFuncReturn != nil {
			ch.collectFuncReturn(ctx, callRes)
		}
		ctx.Next()
	})
	// after逻辑
	ctx.chain = append(ctx.chain, ch.after...)

	return ctx
}

// CombineSrvChain 组合对应服务与中间件
// t 传入的是要加中间件的服务方法（需要为函数类型或者方法类型）
// 函数会将t动态更改为带着上下文ctx与前置后置逻辑的函数，然后按照完整的函数签名返回
func CombineSrvChain[T any](fil *NormalChain, t T) T {
	// 获取函数的反射值并校验
	// 构造调用ctx与构造新的代理函数
	// 返回代理函数
	return WrapChainAsFunc(fil, t)
}

func (ch *NormalChain) BeforeInvoke(h ...Filter) {
	ch.before = h
}

func (ch *NormalChain) AfterInvoke(h ...Filter) {
	ch.after = h
}

func (ch *NormalChain) SetCollectFn(arg ValuerCollector, res ValuerCollector) {
	ch.collectArg = arg
	ch.collectFuncReturn = res
}
