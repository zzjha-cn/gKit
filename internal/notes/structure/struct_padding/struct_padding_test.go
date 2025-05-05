package structpadding_test

import (
	"fmt"
	"testing"
	"unsafe"
)

/* 在CPU底层，每一次读取都是按照8的倍数读取二进制数据，如果这个流程中读取到不需要的数据，则需要多一些计算与剔除的过程
所以，为了加快计算，可以通过填充结构体——padding struct，形成8的倍数，对硬件更加友好。
*/
// 举例：程序严格要求主动填充的时候
// type mstats struct {
// 	// ... ... ]
// 	// Add an uint32 for even number of size classes to align below fields
// 	// to 64 bits for atomic operations on 32 bit platforms.
// 	_ [1 - _NumSizeClasses%2]uint32 // 这里做了主动填充
// 	last_gc_nanotime uint64 // last gc (monotonic time)
// 	last_heap_inuse uint64 // heap_inuse at mark termination of the previous GC
// 	// ... ...
// }

type TestModel struct {
	F1 uint64 // 8 byte
	F2 uint16 // 2 byte
	F3 byte   // 1 byte
}

type TestModel2 struct {
	F1 byte   // 1 byte
	F2 uint64 // 8 byte
	F3 uint16 // 2 byte
}

func TestPrintStructPadding(t *testing.T) {

	m := TestModel2{}
	calStructCap(m)
	fmt.Println("unsafe sizeof:", unsafe.Sizeof(m))
	fmt.Println("F3 偏移量:", unsafe.Offsetof(m.F3))

}

// 注意，转换接口的话，实际占用字节大小会变化. 因为接口类型和具体类型不一样的
func calStructCap(t any) {
	fmt.Println("占用内存的大小(any)：", unsafe.Sizeof(t))                    // 接口表示输出的就是16个字节
	fmt.Println("占用内存的大小(TestModel)：", unsafe.Sizeof(t.(TestModel2))) // 具体类型输出的还是24个字节
	fmt.Println("字段相当于变量t其实位置的偏移量：", unsafe.Offsetof(t.(TestModel2).F3))
}
