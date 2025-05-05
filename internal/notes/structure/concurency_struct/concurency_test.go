package concurency_struct

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* 并发更新struct的属性 */
// 在并发的时候，如果更新结构体字段，容易出现多写与复写的情况，
// 避免出现字段更新了一部分，但是被另外的程序使用了，期望在使用的时候是原子更新
// 如何在并发场景下原子性更新结构体，并有良好的性能？

type User struct {
	Name string
	Age  int
}

// 方案一：加锁
type UserManagerV1 struct {
	user *User
	mu   sync.RWMutex
}

func (u *UserManagerV1) doSomething() {
	u.mu.Lock()
	// defer u.mu.Unlock()
	// 这里defer不可以sleep了，因为sleep会导致defer执行，这个协程可以被强占
	// （没有锁住）

	u.user.Name = "new name"
	// 假设存在长逻辑
	// time.Sleep(1 * time.Second)
	u.user.Age = 30
	u.mu.Unlock()
}

func (u *UserManagerV1) doSomethingNoLock() {
	u.user.Name = "new name"
	// 假设存在长逻辑
	time.Sleep(1 * time.Second)
	u.user.Age = 3
}

func TestTransUser(t *testing.T) {
	manager := &UserManagerV1{
		user: &User{Name: "old name", Age: 1},
	}

	cnt := 10
	wg := sync.WaitGroup{}

	t.Log("\ntest with locker")
	now := time.Now()
	for i := 0; i < cnt; i++ {
		wg.Add(1)
		go func() {
			fmt.Printf("[%s,%d]\n", manager.user.Name, manager.user.Age)
			manager.doSomething()
			// manager.doSomethingNoLock()
			wg.Done()
		}()
	}
	wg.Wait()
	t.Logf("done \nuse time %d ms", time.Now().Sub(now).Milliseconds())
}

// 可以看到加锁会导致并发变成串行执行，性能很差
// 能不能直接创建一个user，计算完成后再覆盖原本的user呢？
// 不能，因为即使覆盖了原本的user，拿到旧user的协程还是在处理旧数据
// 新的更新内容并没有辐射到别的协程
type UserManagerV2 struct {
	user *User
}

func (u *UserManagerV2) doSomethingNoLock() {
	us := &User{}
	us.Name = "new name"
	// 假设存在长逻辑
	us.Age = 3
	u.user = us
}

func TestTransUserV2(t *testing.T) {
	manager := &UserManagerV2{
		user: &User{Name: "old name", Age: 1},
	}

	cnt := 10
	wg := sync.WaitGroup{}

	t.Log("\ntest with locker")
	now := time.Now()
	for i := 0; i < cnt; i++ {
		wg.Add(1)
		go func() {
			fmt.Printf("[%s,%d]\n", manager.user.Name, manager.user.Age)
			manager.doSomethingNoLock()
			wg.Done()
		}()
	}
	wg.Wait()
	t.Logf("done \nuse time %d ms", time.Now().Sub(now).Milliseconds())
}

// 那么就在user上套多一层指针，通过atomic原子更新

type UserManagerV3 struct {
	store atomic.Pointer[User]
}

func (u *UserManagerV3) doSomethingNoLock() {
	us := &User{}
	us.Name = "new name"
	// 假设存在长逻辑
	us.Age = 3
	u.store.Store(us)
}

func TestTransUserV3(t *testing.T) {
	manager := &UserManagerV3{}

	manager.store.Store(&User{Name: "old name", Age: 1})

	cnt := 10
	wg := sync.WaitGroup{}

	t.Log("\ntest with locker")
	now := time.Now()
	for i := 0; i < cnt; i++ {
		wg.Add(1)
		go func() {
			u := manager.store.Load()
			fmt.Printf("[%s,%d]\n", u.Name, u.Age)
			manager.doSomethingNoLock()
			wg.Done()
		}()
	}
	wg.Wait()
	t.Logf("done \nuse time %d ms", time.Now().Sub(now).Milliseconds())
}
