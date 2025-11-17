package etcd101

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	recipe "go.etcd.io/etcd/client/v3/experimental/recipes"
)

func TestQueue(t *testing.T) {
	cfg := clientv3.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
	}

	etcdClient, _ := clientv3.New(cfg)
	defer etcdClient.Close()

	queueName := "demo_queue"
	qu := recipe.NewQueue(etcdClient, queueName)
	qu.Enqueue("name")
	name, _ := qu.Dequeue()
	fmt.Println("dequeue", name)

	priq := recipe.NewPriorityQueue(etcdClient, queueName)
	priq.Enqueue("name_pri_queue", 3)
}

func TestBarrier(t *testing.T) {
	/*
		一组节点协同工作，共同等待一个信号，在
		信号未出现前，这些节点会被阻塞住，而一旦信号出现，这些阻塞的节点就会同时开始继
		续执行下一步的任务。
	*/

	cfg := clientv3.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
	}

	etcdClient, _ := clientv3.New(cfg)
	defer etcdClient.Close()
	barrierName := "barrier"
	br := recipe.NewBarrier(etcdClient, barrierName)

	go func() {
		fmt.Println("信号创建barrier")
		br.Hold()
		// 模拟条件准备
		time.Sleep(3 * time.Second)
		// 条件准备完毕
		br.Release()
	}()
	for i := 0; i < 5; i++ {
		go func() {
			fmt.Println("子任务启动并等待执行")
			br.Wait()
			fmt.Println("子任务执行")
		}()
	}

	// 计数型barrier,通过数量控制状态
	session, _ := concurrency.NewSession(etcdClient)
	doublebr := recipe.NewDoubleBarrier(session, barrierName, 5)
	for i := 0; i < 5; i++ {
		go func(ind int) {
			fmt.Println("计数协程开始", ind)
			doublebr.Enter() // 阻塞直到enter数量达到设定值，才统一开始
			time.Sleep(2 * time.Second)
			fmt.Println("协程完成", ind)
			doublebr.Leave() // 阻塞直到大家都已经完成并调用leave
			fmt.Println("结束", ind)
		}(i)
	}
}

func TestSTM(t *testing.T) {
	cfg := clientv3.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
	}

	etcdClient, _ := clientv3.New(cfg)
	defer etcdClient.Close()

	// 批量操作多个key与执行多个任务

	totalAccounts := 5
	for i := 0; i < totalAccounts; i++ {
		k := fmt.Sprintf("accts/%d", i)
		if _, err := etcdClient.Put(context.TODO(), k, "100"); err != nil {
			log.Fatal(err)
		}
	}

	// 核心逻辑(批量动作)
	exchange := func(stm concurrency.STM) error {
		from := rand.Intn(5)
		to := rand.Intn(5)
		if from == to {
			return nil
		}
		fromK, toK := fmt.Sprintf("accts/%d", from), fmt.Sprintf("accts/%d", to)
		fromV, toV := stm.Get(), stm.Get(fmt.Sprintf("accts/%d", to))
		fromInt, toInt := 0, 0
		fmt.Sscanf(fromV, "%d", &fromInt)
		fmt.Sscanf(toV, "%d", &toInt)

		// 把源账号一半的钱转账给目标账号
		xfer := fromInt / 2
		fromInt, toInt = fromInt-xfer, toInt+xfer
		// 把转账后的值写回
		stm.Put(fromK, fmt.Sprintf("%d", fromInt))
		stm.Put(toK, fmt.Sprintf("%d", toInt))
		return nil
	}

	// 并发执行
	var wg = sync.WaitGroup{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := concurrency.NewSTM(etcdClient, exchange)
			if err != nil {
				fmt.Println(err)
			}
		}()
	}

	wg.Wait()

	// 验证总量
	var sum = 0
	// for i := 0; i < totalAccounts; i++ {
	// 	val, err := etcdClient.Get(context.TODO(), fmt.Sprintf("accts/%d", i))
	// 	if nil != err {
	// 		continue
	// 	}
	// 	val.Kvs[0].Value
	// }
	res, _ := etcdClient.Get(context.TODO(), "accts/", clientv3.WithPrefix())
	for _, kv := range res.Kvs {
		v := 0
		fmt.Sscanf(string(kv.Value), "%d", &v)
		sum += v
	}
	fmt.Println(sum)
}
