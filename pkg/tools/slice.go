package tools

import "reflect"

// 将list转化为别的结构
func MapSlice[T any, V any](list []T, fn func(t T, v *V) (isBreak bool)) V {
	var (
		res         = new(V)
		zero        V
		isEmptyList bool = true
	)

	// map不能通过new初始化，如果slice转为map，需要特殊处理
	if isMap(*res) {
		// 创建一个空的 map
		typ := reflect.TypeOf(res).Elem()
		vShadow := reflect.MakeMapWithSize(typ, 0).Interface().(V)
		res = &vShadow
	}

	for _, t := range list {
		isEmptyList = false
		if stop := fn(t, res); stop {
			break
		}
	}
	return If(isEmptyList, zero, *res)
}

// InsertToSlice 往切片中插入值
// [0 1 2 3 4 5 6 7]   +  idx=4 ,v=-1   ==>   [0 1 2 3 -1 4 5 6 7]
func InsertToSlice[T any](s []T, idx int, v T) []T {
	if idx < 0 {
		idx = 0
	} else if idx > len(s) {
		idx = len(s)
	}
	out := make([]T, len(s)+1)
	copy(out, s[:idx])
	out[idx] = v
	copy(out[idx+1:], s[idx:])
	return out
}

func InitialSlice[T any](length int, f func() T) []T {
	var res = make([]T, 0, length)
	for i := 0; i < length; i++ {
		res = append(res, f())
	}
	return res
}
