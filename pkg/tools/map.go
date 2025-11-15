package tools

import "reflect"

// isMap 判断一个值是否是 map 类型
func isMap(v interface{}) bool {
	return reflect.ValueOf(v).Kind() == reflect.Map
}

// map转化为别的结构
func MapTo[K comparable, V any, T any](m map[K]V, fn func(k K, v V, t *T) (isBreak bool)) T {
	var (
		res *T = new(T)
	)
	// map不能通过new初始化
	if isMap(*res) {
		// 创建一个空的 map
		typ := reflect.TypeOf(res).Elem()
		vShadow := reflect.MakeMapWithSize(typ, 0).Interface().(T)
		res = &vShadow
	}
	for k, v := range m {
		if fn(k, v, res) {
			break
		}
	}

	return *res
}
