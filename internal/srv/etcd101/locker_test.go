package etcd101

import (
	"context"
	"fmt"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	recipe "go.etcd.io/etcd/client/v3/experimental/recipes"
)

func TestEtcdLocker(t *testing.T) {
	cfg := clientv3.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
	}
	lockString := "srv1_locker"

	cli, _ := clientv3.New(cfg)
	defer cli.Close()

	session, _ := concurrency.NewSession(cli)
	defer session.Close()

	// 返回标准的mutex locker
	locker := concurrency.NewLocker(session, lockString)

	locker.Lock()
	fmt.Println("加锁中")
	locker.Unlock()
	fmt.Println("解锁完成")

	// etcd底下实际的互斥锁，可以传递context超时控制
	mutexLocker := concurrency.NewMutex(session, lockString)
	mutexLocker.Lock(context.Background())
	mutexLocker.Unlock(context.TODO())

	// 分布式读写锁
	rwLocker := recipe.NewRWMutex(session, lockString)
	rwLocker.RLock()
	rwLocker.RUnlock()

	rwLocker.Lock()
	rwLocker.Unlock()
}
