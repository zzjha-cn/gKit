package listmaker

import (
	"context"
	"sync/atomic"
)

// 将list转化为递归，形成责任链调用
// - 将一组相同签名的函数转化为递归调用
// =》通过中间层，将不同签名的函数转为相同的签名然后实现递归调用
// =》将一组同接口的实现转化递归模式

func CommonRecursively[T func() error](list []T) error {
	if len(list) <= 0 {
		return nil
	}

	var exec func(index int) error
	exec = func(index int) error {
		if index >= len(list) {
			return nil
		}
		e1 := list[index]()
		if e1 != nil {
			return e1
		}

		return exec(index + 1)
	}
	return exec(0)
}

// ---------------------------------------------------------

// 抽象为接口

type (
	Context struct {
		Ctx    context.Context
		Index  atomic.Uint32
		Err    error
		Result any
		// Collect
	}

	// 实现了这个接口，就可以让模块帮忙构造责任链
	IList2Recursive interface {
		Exec(ctx *Context) (isBreak bool) // 执行
	}

	Collect[T any] func(ctx *Context) (T, error)
)

func RecursivelyWithImpl[T IList2Recursive](list []IList2Recursive, ctx *Context) {
	var length = len(list)
	var eFn func(index int, ctx *Context)
	eFn = func(index int, ctx *Context) {
		if index >= length {
			return
		}
		isBreak := list[index].Exec(ctx)
		if isBreak {
			return
		}

		ctx.Index.Add(1)
		eFn(index+1, ctx)
	}

	eFn(0, ctx)
}
