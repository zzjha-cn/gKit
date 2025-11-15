package test

import (
	"fmt"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/tools"
)

func TestMapSlice(t *testing.T) {
	var a = []int{1, 2, 3, 4, 5}

	// 转化为别的类型的slice
	b := tools.MapSlice[int, []int64](a, func(t int, v *[]int64) (isBreak bool) {
		(*v) = append((*v), int64(t)+1)
		return
	})
	fmt.Println(b)

	// slice转化为map
	c := tools.MapSlice[int, map[int]int](a, func(t int, v *map[int]int) (isBreak bool) {
		(*v)[t] = t + 1
		return
	})
	fmt.Println(c)

	// 如果a是结构体数组，可以带来更多可操作性
}

func TestMapTo(t *testing.T) {
	a := map[string]int{
		"L1": 1,
		"L2": 2,
		"L3": 3,
	}

	b := tools.MapTo[string, int, []int](a, func(k string, v int, t *[]int) (isBreak bool) {
		(*t) = append((*t), v)
		return
	})
	fmt.Println(b)

	type st struct {
		key   string
		value int
	}
	c := tools.MapTo[string, int, []st](a, func(k string, v int, t *[]st) (isBreak bool) {
		(*t) = append((*t), st{key: k, value: v})
		return
	})
	fmt.Println(c)

}
