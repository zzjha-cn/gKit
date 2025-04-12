package test

import (
	"fmt"
	"gKit/pkg/chain"
	"reflect"
	"testing"
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

		ch := chain.NewFilterChain()
		ch.BeforeInvoke(chain.RecoveryFilter, chain.TimeQueryFilter)
		ch.AfterInvoke(chain.StopFilter)
		ch.SetTansferFn(func(ctx *chain.ChainContext, args []reflect.Value) error {
			ctx.Args = []any{args[0].Interface()}
			return nil
		}, nil)

		get := chain.CombineSrvChain(ch, s.GetId)
		id := get("name")
		fmt.Println(id)
	})
}
