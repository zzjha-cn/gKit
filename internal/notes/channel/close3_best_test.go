package channel

import (
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestCloseBest(t *testing.T) {
	type testCase struct {
		name string
		fn   func()
	}

	tcList := []*testCase{
		{
			name: "单发送者 单接受者",
			fn:   mode1,
		},
		{
			name: "单发送者 多接受者",
			fn:   mode2,
		},
		{
			name: "多发送者 多接受者",
			fn:   mode3,
		},
		{
			name: "多发送者 多接受者（try send）",
			fn:   mode33,
		},
		{
			name: "单发送者 多接受者，但是由第三方关闭",
			fn:   mode4,
		},
		{
			name: "多发送者 多接受者，需要通知的情况",
			fn:   mode5,
		},
	}

	for _, tc := range tcList {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn()
		})
	}

}

// 粗暴关闭(close1)的处理只能说是补丁，通过defer recover  处理
// 礼貌关闭(close2)的处理是一种实现，通过锁的机制处理
// 但是，粗暴关闭是会中断程序，这样不符合通道关闭的原则，并且这个样子会造成逻辑上的冲突
// 礼貌关闭是通过锁，那不可避免会出现数据竞争，降低了效率

// 有人认为不应该使用defer recover  还有 sync 处理通道关闭的问题

const Max = 100000
const NumReceiver = 100

// 最简单的情况，一个发送者对1个接收者--(只需要在不想发送的时候关闭就可以)
func mode1() {
	// 定义相关的随机功能
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(0)

	wgreceivers := sync.WaitGroup{}
	wgreceivers.Add(NumReceiver)
	datach := make(chan int)
	// 所谓的一个，就是只有一个管道了，然后多次发送之后关闭而已

	// sender
	go func() {
		for {
			if value := rand.Intn(Max); value == 0 {
				close(datach)
				return
			} else {
				datach <- value
			}
		}
	}()

	// receiver
	for i := 0; i < NumReceiver; i++ {
		go func() {
			defer wgreceivers.Done()
			for value := range datach {
				log.Println(value)
			}
		}()
	}

	// 那么问题来了，这个为什么放这里的？--就是等待就是等接收者都结束
	wgreceivers.Wait()
}

// 这种一对一的情况，其实就是在发送方关闭通道就行了

// 第二种情况，多个发送者对一个接收者
// 这个时候其实不用关闭传输通道datach，因为只要没有数据了，没有协程引用，就会自动垃圾回收
func mode2() {
	wgreceivers := sync.WaitGroup{}
	wgreceivers.Add(1) // 注意这个只是添加上了1

	datach := make(chan int)
	stopch := make(chan struct{}) // 这个其实就是一个信号，

	// sender
	for i := 0; i < NumReceiver; i++ {
		go func() {
			for {
				select {
				case <-stopch:
					return
				default:
					// 为什么会有这么奇怪的写法
					// 其实也就是说不让上面一直阻塞着
				} // 这个select仅仅只是为了在当前例子增加stopch分支的概率

				// 其实下面这样写还是有点小问题的
				// 这个即使stopch被关闭了，不一定就会立刻执行这个reutrn。有可能还是往datach里面填值，不过这里对于这个例子，还是没有影响的
				select {
				case <-stopch:
					return
				case datach <- rand.Intn(Max):
				}
				// 接收stop复写两次，增加了检测到的概率
			}
		}()
	}

	// receivers
	go func() {
		defer wgreceivers.Done()
		// 因为上面的add就加了一个 ， 这里接收结束就可以直接done掉

		for i := range datach {
			if i == Max-1 { // 等到检测到某一个随机值和这个相等，就直接关闭stopch，然后上面的发送方就会返回（不再发送）——当然，照上面的逻辑，不会立刻的
				close(stopch)
				// 本来原则是发送端关闭的，但是，现在只有一个接受端，所以直接通过这个告诉几个发送端说不用再发了，结束了
				// 这里主动关闭了相关stopch，这样会导致上面的发送方其实一下子就都return——不会立刻，因为select是随机的
				return
			}
			fmt.Println(i)
		}
	}()

	// 等到receiver关闭了stopch，然后return。会done，这里的等待也就到了尽头
	wgreceivers.Wait()
}

// 第三种情况，多个发送者，多个接收者——其实借助的只是一条管道而已
// 发送者方都要知道彼此是不是都已经发送完毕，然后大家都停止发送之后，才可以关闭通道
// 接收者与发送者双方还是都不可以关闭通道，需要外界开试着关闭通道
func mode3() {
	rand.Seed(time.Now().UnixNano())

	numSenders, numReceivers := 1000, 10

	wgreceivers := sync.WaitGroup{}
	wgsenders := sync.WaitGroup{}

	datach := make(chan int)
	stopch := make(chan struct{})

	ss := false // 通过这个变量，控制是不是需要关闭发送端了
	// senders
	for i := 0; i < numSenders; i++ {
		wgsenders.Add(1)
		go func() {
			for {
				value := rand.Intn(Max)
				if !ss && value == Max-1 {
					close(stopch)
					ss = true
				}
				// 上面这样你就有个隐患了，因为这样子会出现重复关闭channel的情况。但是，这个思路应该是正确的
				select {
				case <-stopch:
					wgsenders.Done()
					return

				// case datach <- rand.Intn(Max):
				case datach <- value:

				}
			}
		}()
	}

	// 监视看看sender有没有都已经关闭了
	go func() {
		// 这个其实就是闭包了吧 ，匿名函数可以使用函数内部的变量
		wgsenders.Wait()
		fmt.Println("所有的生产者已经关掉")
		close(datach)
	}()

	// receivers
	wgreceivers.Add(numReceivers)
	for i := 0; i < numReceivers; i++ {
		go func() {
			defer wgreceivers.Done()
			for value := range datach {
				if value == Max-1 {
					close(stopch)
					return
				}
				fmt.Println(value)
			}
		}()
	}
	wgreceivers.Wait()
}

// 第三种情况的另外实现
// 这里引入了try send的思想
func mode33() {
	rand.Seed(time.Now().UnixNano())

	numReceivers, numSenders := 10, 1000
	wgReceivers := sync.WaitGroup{}
	datach := make(chan int)
	stopch := make(chan struct{})

	tostop := make(chan string, 1) // 注意这里的1，这里是带缓存的.目的是为了防止信号丢失
	// tostop其实就是监听信号，如果里面有值了，那就表示从系统中接受到了关闭信号
	var stoppedBy string // 这个就是关闭信号的具体承载

	// 这个其实就是控制等待的，要等接收者都处理完毕了，才等待结束
	wgReceivers.Add(numReceivers)

	// 监控的情况
	go func() {
		stoppedBy = <-tostop // 里面没有值就会一直阻塞着
		close(stopch)
	}()

	// senders
	// 有几个数量就会启动几个协程，然后没有触发条件就卡住，触发了条件的话，传值给tostop，然后负责监控的协程执行关闭
	// 执行关闭之后剩下的sender会在for的控制下，直接关闭退出
	for i := 0; i < numSenders; i++ {
		go func(id string) {
			for {
				value := rand.Intn(Max)
				// 这个就是在发送端关闭通道
				if value == 0 {
					// 下面这个就是try-send
					select {
					case tostop <- "send#" + id:
					default:
					}
					return
				}

				select {
				case <-tostop:
					return
				case datach <- value:
				}
			}
		}(strconv.Itoa(i))
	}

	// receivers
	// 思路和上面差不多，然后这里面也是有条件触发关闭指令的
	for i := 0; i < numReceivers; i++ {
		go func(id string) {
			defer wgReceivers.Done()
			for {
				select {
				case <-stopch:
					return
				default:
				}

				select {
				case <-stopch:
					return
				case value := <-datach:
					if value == Max-1 {
						select {
						case tostop <- "receiver#" + id:
						default:
						}
					}
					fmt.Println(value)

				}
			}

		}(strconv.Itoa(i))
	}

	wgReceivers.Wait()
	fmt.Println("stop by", stoppedBy)

	// 另外的改良方法
	// 还可以将tostop的缓冲区设置为numSenders + numReceivers
	// 这样就不用try-send select块来通知调解人
	// if value == 0 { //第一块调解
	// 	toStop <- "sender#" + id
	// 	return
	// }
	// 第二块
	// if value == Max-1 {
	// 	toStop <- "receiver#" + id
	// 	return
	// }
}

// 第四种情况，多个接收者，一个发送者  -- 关闭请求由第三方协程发出
func mode4() {
	rand.Seed(time.Now().UnixNano())

	num3 := 15 // 第三方调用的模拟

	wgreceivers := sync.WaitGroup{}
	wgreceivers.Add(NumReceiver)

	datach := make(chan int)
	// 为什么既closing 还有 closed ？--毕竟是多个接收，关闭过程会延长，正在关闭，然后就已经关闭
	closing := make(chan struct{})
	closed := make(chan struct{})

	stop := func() {
		select {
		case closing <- struct{}{}:
			<-closed
		case <-closed:
		}
	}

	// 第三方的协程 控制与传参
	for i := 0; i < num3; i++ {
		go func() {
			value := rand.Intn(3) + 1
			time.Sleep(time.Duration(value) * time.Second)
			stop() // 发出关闭指令
		}()
	}

	// senders
	go func() {
		// 注意，这里有defer
		defer func() {
			close(closed) // closing 就是将相关的协程逐个关闭。 等到了这里的时候，就是closing执行完毕，然后直接关闭这个closed
			close(datach) // 是在发送端关闭
		}()

		for {
			select {
			case <-closing:
				return
			default:
			}

			select {
			case <-closing:
				return
			case datach <- rand.Intn(Max):
			}
		}
	}()

	// receivers
	for i := 0; i < NumReceiver; i++ {
		go func() {
			defer wgreceivers.Done()
			for v := range datach {
				fmt.Println(v)
			}
		}()
	}

	wgreceivers.Wait()
}

// 第五种情况，多个发送方与多个接收方的一种情况：
// 要求：数据通道必须关闭，然后告诉接收方数据发送已经结束
// 这个时候可以引入一个中间信道，将多对多的关系引入到1对1的情况
func mode5() {
	rand.Seed(time.Now().UnixNano())

	// numSenders, numReceivers, num3 := 1000, 10, 15
	numSenders := 1000
	wgreceivers := sync.WaitGroup{}
	wgreceivers.Add(10)

	datach := make(chan int)
	closed := make(chan struct{})
	closing := make(chan string)
	middlech := make(chan int)

	var stoppedBy string

	stop := func(by string) {
		select {
		case closing <- by:
			<-closed
		case <-closed:
		}
	}

	// 中间的情况
	// 这里面怎么体现1对1的情况？
	go func() {
		exit := func(v int, needSend bool) { // 这个函数负责关闭掉信号通道还有数据通道--也是在发送方调用的
			close(closed)
			if needSend { // 需要发送
				datach <- v
			}
			close(datach)
		}

		for {
			select {
			case stoppedBy = <-closing: // 要是检测得到closing的指令
				exit(0, false)
				return
			case v := <-middlech: // 要是检测得到中间信号管道中的信号
				select {
				case stoppedBy = <-closing:
					exit(0, false)
					return
				case datach <- v: // 没有就将信号放入到数据管道继续发送
				}
			}
		}
	}()

	// 就相当于在多个接收者和多个发送者之间加上一个中介，然后发送的数据都打到中介上，只要中介关了，则接收方自然就收不到数据了
	// 但是这样就得考虑效率的问题了，热点的情况

	// 增加一些第三方的协程
	for i := 0; i < 10; i++ {
		go func(id string) {
			r := 1 + rand.Intn(3)
			time.Sleep(time.Duration(r) * time.Second)
			stop("第三方协程：" + id)
		}(strconv.Itoa(i))
	}

	// senders
	for i := 0; i < numSenders; i++ {
		go func(id string) {
			for {
				value := rand.Intn(Max)
				if value == 0 {
					stop("sender#" + id) // 某一个触发结束的条件--执行的是stop
					return
				}

				select {
				case <-closed:
					return
				default:
				}

				select {
				case <-closed:
					return
				case middlech <- value: // 这个就是不同了，往中间件里写东西
				}
			}
		}(strconv.Itoa(i))
	}

	// receivers , 10个接收方差不多
	for range [10]struct{}{} {
		go func() {
			defer wgreceivers.Done()

			for value := range datach { // 取出东西来输出，直到所有接收者都退出了，执行defer done掉所有
				fmt.Println(value)
			}
		}()
	}

	// ...
	wgreceivers.Wait()
	fmt.Println("stopped by", stoppedBy)
}
