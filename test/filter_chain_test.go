package test

import (
	"context"
	"fmt"
	"gKit/pkg/chain"
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

func TestChain(t *testing.T) {
	t.Run("use_chain_filter", func(t *testing.T) {
		s := &Server{
			Id: "use_filter_chain",
		}

		ch := chain.NewNormalChain()
		ch.BeforeInvoke(RecoveryFilter, TimeQueryFilter)
		ch.AfterInvoke(StopFilter)
		ch.SetCollectFn(func(ctx *chain.NormalCtx, args []reflect.Value) error {
			ctx.Args = []any{args[0].Interface()}
			return nil
		}, nil)

		get := chain.CombineSrvChain(ch, s.GetId)
		id := get("name")
		fmt.Println(id)
	})
}

func TimeQueryFilter(ctx *chain.NormalCtx) {
	ti := time.Now()
	ctx.Next()
	fmt.Printf("query[%s] mills:%dms\n", ctx.MethodName, time.Since(ti).Milliseconds())
}

func StopFilter(ctx *chain.NormalCtx) {
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

func RecoveryFilter(ctx *chain.NormalCtx) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Printf("panic [%s] (%v)", ctx.MethodName, rec)
		}
	}()
	ctx.Next()
}
