package etcd101

import (
	"context"
	"fmt"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
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

// 测试etcd事务
func TestTx(t *testing.T) {
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

	// 转账100相关
	from := "id=1"
	to := "id=2"
	// 事务获取两个的人的货币余额
	tnxResp, _ := etcdClient.Txn(context.TODO()).Then(clientv3.OpGet(from), clientv3.OpGet(to)).Commit()

	fromValue := tnxResp.Responses[0].GetResponseRange().Kvs[0]
	toValue := tnxResp.Responses[1].GetResponseRange().Kvs[1]

	// 随便假设一个初始条件
	if string(fromValue.Value) != string(toValue.Value) {
		return
	}

	tnxResp, _ = etcdClient.Txn(context.TODO()).
		If(
			clientv3.Compare(clientv3.ModRevision(from), "=", fromValue.ModRevision),
			clientv3.Compare(clientv3.ModRevision(to), "=", toValue.ModRevision),
		).
		Then(
			clientv3.OpPut(from, "0"), // 扣费
			clientv3.OpPut(to, "200"), // 增加
		).
		Commit()

	return
}

// 测试租约，类似分布式抢锁机制
func TestLease(t *testing.T) {
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

	// 创建
	lease := clientv3.NewLease(etcdClient)
	grant, _ := lease.Grant(context.TODO(), 10)             // 10秒的租约
	ch, _ := etcdClient.KeepAlive(context.TODO(), grant.ID) // 自动续约
	go func() {
		for range ch {

		}
	}()

	lockerKey := "app/lock"
	curName := "v1"

	txResp, _ := etcdClient.Txn(context.TODO()).
		If(
			clientv3.Compare(clientv3.CreateRevision(lockerKey), "=", 0),
		).
		Then(
			// set进去的同时，需要维护租约，毕竟这个是一个锁，防止自己挂了无法释放锁
			clientv3.OpPut(lockerKey, curName, clientv3.WithLease(grant.ID)),
		).
		Else(
			clientv3.OpGet(lockerKey),
		).Commit()

	if txResp.Succeeded {
		fmt.Println("加锁成功")
	} else {
		fmt.Println("加锁失败，被占用: ", string(txResp.Responses[0].GetResponseRange().Kvs[0].Value))
	}
}
