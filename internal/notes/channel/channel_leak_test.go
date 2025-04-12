package channel

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestExam(t *testing.T) {
	// 情况一  输出是什么呢？
	TestLeakOfMemory1(t)
	// out:
	// NumGoroutine: 2
	// 超时
	// NumGoroutine: 3
	// 这里面一个协程没有退出，一直阻塞，并且没有回收，如果在循环中使用这样的代码，会吃内存，容易OOM

	// 但是上面的情况即使加上了缓存，还要求收发的数量一样，不然，还是会内存泄漏
	// 出现了第二种情况
	fmt.Println("NumGoroutine:", runtime.NumGoroutine())
	chanLeakOfMemory2()
	time.Sleep(time.Second * 3) // 等待 goroutine 执行，防止过早输出结果
	fmt.Println("NumGoroutine:", runtime.NumGoroutine())
	// 加了超时机制，接收者这边直接离开了协程，剩下的生产者还是在阻塞中

	// 所以，解决方式还是应该使用优雅的方式——增加一个额外的stop channel用来终结发送者的chan
	chanLeakOfMemory3()
}

func TestLeakOfMemory1(t *testing.T) {
	fmt.Println("NumGoroutine:", runtime.NumGoroutine())
	chanLeakOfMemory()
	time.Sleep(time.Second * 3) // 等待 goroutine 执行，防止过早输出结果
	fmt.Println("NumGoroutine:", runtime.NumGoroutine())
}

func chanLeakOfMemory() {
	errCh := make(chan error) // (1)
	go func() {               // (5)
		time.Sleep(2 * time.Second)
		errCh <- errors.New("chan error") // (2)  这个就会一直阻塞着，不会回收，也没有东西可以结束阻塞
	}()

	var err error
	select {
	case <-time.After(time.Second): // (3) 大家也经常在这里使用 <-ctx.Done()
		fmt.Println("超时")
	case err = <-errCh: // (4)  这里有执行之后，（2）才能够执行，但是，上面会直接超时跳过
		if err != nil {
			fmt.Println(err)
		} else {
			fmt.Println(nil)
		}
	}
} // 所以最终的原因也就是没有带缓冲的chan--除非增加有缓存的

// ====================================================

func chanLeakOfMemory2() {
	ich := make(chan int, 100) // (3)
	// sender
	go func() {
		defer close(ich)
		for i := 0; i < 10000; i++ {
			ich <- i
			time.Sleep(time.Millisecond) // 控制一下，别发太快
		}
	}()
	// receiver
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for range ich { // (2)
			if ctx.Err() != nil { // (1)
				fmt.Println(ctx.Err())
				return // 退出后，发送端还在发送
			}
			// fmt.Println(i)
		}
	}()
} // 就是超时机制，接收者这边直接离开了协程，剩下的生产者还是在阻塞中

// ====================================================
func chanLeakOfMemory3() {
	ich := make(chan int, 100) // (3)
	tostop := make(chan struct{})

	// sender
	go func() {
		defer close(ich)
		for i := 0; i < 10000; i++ {
			// ich <- i
			select {
			case <-tostop:
				close(ich)
				return
			case ich <- i:
			}
			time.Sleep(time.Millisecond) // 控制一下，别发太快
		}
	}()
	// receiver
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for i := range ich { // (2)
			if ctx.Err() != nil { // (1)
				fmt.Println(ctx.Err())
				close(tostop)
				return // 这里直接退出了，生产者那边却还发送
			}
			fmt.Println(i)
		}
	}()
}
