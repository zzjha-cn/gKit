package lifttime_test

import (
	"fmt"
	"runtime"
	"testing"
)

/* 生命周期
内存结构的生命周期 -- 分配 使用与释放
资源对象的生命周期 -- 创建与释放
*/
// 可以使用析构函数,来作为对象生命周期结束时候的回调
// func SetFinalizer(obj interface{}, finalizer interface{})
// obj需要为指针类型 , finalizer 需要是一个函数,并且参数为obj的类型,无返回值

func TestShowLifttime(t *testing.T) {
	// T1()
	// runtime.GC()

	// T2()

	fmt.Println(T3())
	runtime.GC()
}

func T1() {
	a := 1234

	runtime.SetFinalizer(&a, func(*int) {
		fmt.Println("T1 函数内变量发生回收")
	})
}

func T2() {
	var a = 12345

	runtime.SetFinalizer(&a, func(*int) {
		fmt.Println("T2 函数执行中变量发生回收")
	})
	runtime.GC()
}

func T3() *int {
	var a = 1234

	runtime.SetFinalizer(&a, func(*int) {
		fmt.Println("T3 函数返回的变量指针发生回收")
	})

	return &a
}
