package chain

import (
	"context"
)

type (
	NormalCtx struct {
		MethodName string
		// 方法入参
		Args []any
		// 方法响应
		FuncReturn []any

		standardCtx context.Context

		chain    []Filter
		curIndex int

		Builder *CtxBuilder
	}

	// 中间件组链方式有多种：
	// - 像Gin一样通过ctx传递调用数组与index，一层层调用
	// - 使用闭包将所有中间件构造成责任链模式(通过顺序编排)

	// 是否要允许用户自定义？不许，因为收益太低。选择gin框架的实现方案。

	// TODO ctx的pool
)

func (c *NormalCtx) Name() string {
	return c.MethodName
}

func (c *NormalCtx) GetArg() []any {
	return c.Args
}

func (c *NormalCtx) GetResult() []any {
	return c.FuncReturn
}

func (c *NormalCtx) Set(s string, a any) {
	c.standardCtx = context.WithValue(c.standardCtx, s, a)
}

func (c *NormalCtx) Get(s string) any {
	return c.standardCtx.Value(s)
}

func NewNormalCtx() *NormalCtx {
	return &NormalCtx{
		standardCtx: context.Background(),
	}
}

func (c *NormalCtx) Next() {
	if c.curIndex >= len(c.chain) {
		return
	}
	ind := c.curIndex
	c.curIndex++
	c.chain[ind](c)
}

func (c *NormalCtx) Stop() {
	c.curIndex = len(c.chain)
}

func (c *NormalCtx) GetCtxBuild() *CtxBuilder {
	return c.Builder
}
