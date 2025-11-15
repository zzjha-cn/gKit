package base

import (
	"fmt"
	"testing"

	"golang.org/x/sync/errgroup"
)

// 关于分组执行goroutine的一些示例

func TestErrGroup(t *testing.T) {
	var eg = errgroup.Group{}

	for i := 0; i < 5; i++ {
		index := i
		eg.Go(func() error {
			fmt.Println(index)
			return nil
		})
	}
	eg.Wait()
}
