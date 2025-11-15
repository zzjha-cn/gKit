package cyclic_barrier

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marusama/cyclicbarrier"
)

// 测试一下循环栅栏（基于计数的栅栏）的使用
// 表示调度的内容与数量强相关
// 模拟游戏房间的启动

type player struct {
	name string
}

func TestBarrier(t *testing.T) {
	var list = []player{
		{name: "sean"}, {name: "jean"}, {name: "debian"}, {name: "jim"},
	}

	cy := cyclicbarrier.NewWithAction(len(list)+1, func() error {
		fmt.Println("all ready ,go!")
		return nil
	})

	var wg = sync.WaitGroup{}
	wg.Add(1)
	go func() {
		// 房间管理器初始化
		defer wg.Done()
		time.Sleep(1 * time.Second)
		fmt.Println("room resource ok!")
		cy.Await(context.TODO())
	}()

	for i := 0; i < len(list); i++ {
		wg.Add(1)
		go func(p player) {
			defer wg.Done()
			time.Sleep(300 * time.Millisecond)
			fmt.Printf("play %s ready\n", p.name)
			_ = cy.Await(context.TODO())
		}(list[i])
	}

	wg.Wait()
}
