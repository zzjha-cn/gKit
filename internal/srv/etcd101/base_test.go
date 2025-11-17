package etcd101

import (
	"context"
	"fmt"
	"testing"

	"github.com/coreos/etcd/clientv3"
)

// go get go.etcd.io/etcd/client/v3@v3.5.10

/*
对于简单的、非排他的配置读写，直接使用 clientv3 即可。
但对于需要保证资源互斥访问的分布式锁，
Session 提供的基于 Lease 的自动过期机制是确保系统高可用和防止死锁的关键，
因此在这种场景下必须使用 Session
*/

func TestBase(t *testing.T) {
	cfg := clientv3.Config{
		Endpoints: []string{
			"http://127.0.0.1:2379",
		},
	}

	etcdClient, err := clientv3.New(cfg)
	if err != nil {
		panic(err)
	}
	defer etcdClient.Close()

	// 往集群中设置值
	ctx := context.Background()
	key := "my-test-key1"
	value := "my-test-value1"
	_, err = etcdClient.Put(ctx, key, value)
	if err != nil {
		t.Error(err)
		return
	}

	resp, err := etcdClient.Get(ctx, key)
	if err != nil {
		t.Error(err)
		return
	}
	for _, v := range resp.Kvs {
		fmt.Println(string(v.Key), string(v.Value))
	}
}
