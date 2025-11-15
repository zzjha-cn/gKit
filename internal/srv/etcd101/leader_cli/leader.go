package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/coreos/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/concurrency"
)

var (
	nodeId    = flag.String("nodeid", "", "")
	addr      = flag.String("addr", "http://127.0.0.1:2379", "etcd address")
	electName = flag.String("name", "my-test-elect", "election name")
)

func main() {
	flag.Parse()

	endpoint := strings.Split(*addr, ",")
	cfg := clientv3.Config{
		Endpoints: endpoint,
	}

	etcdClient, err := clientv3.New(cfg)
	if err != nil {
		panic(err)
	}
	defer etcdClient.Close()

	// 获取seesion，如果程序宕机导致session断掉，etcd能检测到
	// Session 是一个抽象概念，它的核心作用是将一个租约（Lease）的生命周期与客户端的连接状态关联起来
	session, err := concurrency.NewSession(etcdClient)
	if err != nil {
		panic(err)
	}

	el := concurrency.NewElection(session, *electName)

	var sc = bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		inputKey := sc.Text()
		switch inputKey {
		case "elect": // 选举命令,如果集群宕机了或者还没有主节点，通过这里开始选举
			go elect(el, *electName)
		case "region": // 辞去leader，重新选举
			region(el, *electName)
		case "query": // 查询目前的leader
			query(el, *electName)
		case "proclaim": // 只更新leader的value
			proclaim(el, *electName)
		case "watch": // 监控leader的变动
			go watch(el, *electName)
		case "rev":
			recv(el, *electName)
		default:
			fmt.Println("unkonw action")
		}
	}
}

var count int

// 选主
func elect(el *concurrency.Election, electName string) {
	log.Println("elect new leader, name=%s", electName)
	// 调用Campaign方法选主,主的值为value-<主节点ID>-<count>
	err := el.Campaign(context.TODO(), fmt.Sprintf("value-%s-%d", *nodeId, count))
	if err != nil {
		log.Println(err)
	}
	log.Println("campaign for Id=%s", *nodeId)
	count++
}

// 重新选主
func region(el *concurrency.Election, electName string) {
	log.Println("region new leader, name=%s", electName)
	if err := el.Resign(context.TODO()); err != nil {
		log.Println(err)
	}
	log.Println("region for Id=%s", *nodeId)
	count++
}

// 为主设置新的值
func proclaim(el *concurrency.Election, electName string) {
	log.Println("proclaim new value for leader, name=%s", electName)
	if err := el.Proclaim(context.Background(), fmt.Sprintf("valuse-%s-%d", *nodeId, count)); err != nil {
		log.Println(err)
	}
	log.Println("proclaim for Id=%s", *nodeId)
	count++
}

// 查询leader的相关信息
func query(el *concurrency.Election, electName string) {
	log.Println("query leader, name=%s", electName)
	resp, err := el.Leader(context.Background())
	if err != nil {
		log.Println(err)
	}
	log.Printf("curent leader is %s=>%s\n", resp.Kvs[0].Key, resp.Kvs[0].Value)
}

// 查看leader版本信息，一般主节点每一次变动都会生成一个版本号
func recv(el *concurrency.Election, electName string) {
	version := el.Rev()
	log.Println("curent version", electName, version)
}
func watch(el *concurrency.Election, electName string) {
	log.Println("watch election", electName)
	ch := el.Observe(context.Background())
	for i := 0; i < 10; i++ {
		resp := <-ch
		log.Printf("leader change to %s:%s\n", resp.Kvs[0].Key, resp.Kvs[0].Value)
	}
}
