package chain

import (
	"context"
	"fmt"
	"time"
)

func TimeQueryFilter(ctx *ChainContext) {
	ti := time.Now()
	ctx.Next()
	fmt.Printf("query[%s] mills:%dms\n", ctx.MethodName, time.Since(ti).Milliseconds())
}

func StopFilter(ctx *ChainContext) {
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

func RecoveryFilter(ctx *ChainContext) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Printf("panic [%s] (%v)", ctx.MethodName, rec)
		}
	}()
	ctx.Next()
}
