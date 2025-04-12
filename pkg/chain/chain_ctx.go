package chain

import "reflect"

type (
	ChainContext struct {
		MethodName string
		// 方法入参
		Args []any
		// 方法响应
		Vals []any

		chain      []FilterHandle
		curIndex   int
		callResult []reflect.Value
	}

	// 中间件组链方式有多种：
	// - 像Gin一样通过ctx传递调用数组与index，一层层调用
	// - 使用闭包将所有中间件构造成责任链模式(通过顺序编排)
	// 是否要允许用户自定义？不许，因为收益太低。选择gin框架的实现方案。

	// 是否使用ctx pool？暂时不用。
)

func NewChainCtx() *ChainContext {
	return &ChainContext{}
}

func (c *ChainContext) Next() {
	if c.curIndex >= len(c.chain) {
		return
	}
	ind := c.curIndex
	c.curIndex++
	c.chain[ind](c)
}

func (c *ChainContext) Stop() {
	c.curIndex = len(c.chain)
}
