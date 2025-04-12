package chain

import (
	"fmt"
	"reflect"
	"testing"
)

type (
	ChainInterface interface {
		MakeChainCtx(v any) *ChainContext
		Next(ctx *ChainContext, ind int)
	}

	FilterHandle func(ctx *ChainContext)

	FilterChain struct {
		chain  []FilterHandle
		before []int
		after  []int

		makeArg func(ctx ChainContext, args []reflect.Value) error
		makeVal func(ctx ChainContext, args []reflect.Value) error
	}

	ChainContext struct {
		MethodName string
		Arg        []any
		Val        []any
		chain      []FilterHandle
	}

	Server struct {
	}
)

func CombineMdChain[T any](fil *FilterChain, t T) T {
	// t 传入的是要加中间件的服务方法
	// 会将t动态代理，更改为带着上下文与前置后置逻辑的函数，然后返回

	// 获取函数的反射值
	fnValue := reflect.ValueOf(t)
	typ := reflect.TypeOf(t)
	if typ.Kind() != reflect.Func {
		return t
	}

	// 创建一个新的函数
	copiedFunc := reflect.MakeFunc(fnValue.Type(), func(args []reflect.Value) []reflect.Value {

		// 调用原始函数
		return fnValue.Call(args)
	})

	return copiedFunc.Interface().(T)
}

func TestUseMiddleware(t *testing.T) {
	s := &Server{}
	d := CombineMdChain(&FilterChain{}, s.GetId)
	id := d("name")
	fmt.Println(id)
}

func TestObj(t *testing.T) {
	s := &Server{}

	fnv := reflect.ValueOf(s).Method(0)
	t1 := reflect.TypeOf(s).Method(0).Type
	t2 := fnv.Type()
	fmt.Println(t1 == t2)
	fmt.Println(t1.String())
	fmt.Println(t2.String())
	fmt.Println(fnv.Type().String())

	v := reflect.ValueOf("666")

	re := fnv.Call([]reflect.Value{v})
	fmt.Println(re[0].Interface())

	cfn := reflect.MakeFunc(fnv.Type(), func(args []reflect.Value) (results []reflect.Value) {
		return fnv.Call(args)
	})

	re = cfn.Call([]reflect.Value{v})
	fmt.Println(re[0].Interface())
}

func (s *Server) GetId(name string) string {
	fmt.Println("srv", name)
	return "asds"
}
