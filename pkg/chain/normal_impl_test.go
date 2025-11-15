package chain

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type (
	Server struct {
		Id string
	}
)

func (s *Server) GetId(name string) string {
	fmt.Println("[GetId]", name)
	return s.Id
}

func TimeQueryFilter(ctx *NormalCtx) {
	ti := time.Now()
	ctx.Next()
	fmt.Printf("query[%s] mills:%dms\n", ctx.MethodName, time.Since(ti).Milliseconds())
}

func StopFilter(ctx *NormalCtx) {
	if len(ctx.Args) > 0 {
		if c, ok := ctx.Args[0].(context.Context); ok {
			if c.Value("STOP_CTX") != nil {
				return
			}
		} else {
			fmt.Println(ctx.Args[0])
		}
	}
	ctx.Next()
}

func RecoveryFilter(ctx *NormalCtx) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Printf("panic [%s] (%v)", ctx.MethodName, rec)
		}
	}()
	ctx.Next()
}

func TestUseFilterChain(t *testing.T) {
	s := &Server{
		Id: "use_filter_chain",
	}

	ch := NewNormalChain()
	ch.BeforeInvoke(RecoveryFilter, TimeQueryFilter)
	ch.AfterInvoke(StopFilter)

	get := CombineSrvChain(ch, s.GetId)
	id := get("name")
	fmt.Println(id)
}

// 测试传递结构体而非传递方法
func TestObject(t *testing.T) {
	s := &Server{
		Id: "id2",
	}

	fnv := reflect.ValueOf(s).Method(0)
	t1 := reflect.TypeOf(s).Method(0).Type
	t2 := fnv.Type()
	fmt.Println("结构体方法类型与值类型是否相等：", t1 == t2)
	fmt.Println("方法类型", t1.String())
	fmt.Println("值类型", t2.String())

	fmt.Println("值调用")
	v := reflect.ValueOf("666")
	valRes := fnv.Call([]reflect.Value{v})
	fmt.Println(valRes[0].Interface())

	fmt.Println("用值的类型创建函数调用")
	cfn := reflect.MakeFunc(fnv.Type(), func(args []reflect.Value) (results []reflect.Value) {
		return fnv.Call(args)
	})
	valRes = cfn.Call([]reflect.Value{v})
	fmt.Println(valRes[0].Interface())
}
