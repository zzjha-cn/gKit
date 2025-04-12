package fast_refelct

import (
	"reflect"
)

/* 优化，重复执行的动作抽出cache，提高整体速度 */
// bugFix: 使用reflect.Value不合理，不应该这样使用
// var cacheV1 map[string]reflect.Value = make(map[string]reflect.Value)

// func optimzeV1(u any, age int) error {
// 	typ := reflect.TypeOf(u)
// 	val := reflect.ValueOf(u)

// 	for typ.Kind() == reflect.Pointer {
// 		typ = typ.Elem()
// 		val = val.Elem()
// 	}

// 	if typ.Kind() != reflect.Struct {
// 		return ErrType
// 	}

// 	var fd reflect.Value
// 	var ok bool
// 	if fd, ok = cacheV1["Age"]; !ok {
// 		fd = val.FieldByName("Age")
// 		cacheV1["Age"] = fd
// 	}
// 	if fd.CanSet() {
// 		fd.SetInt(int64(age))
// 	}
// 	return nil
// }

/* 改进：cache可以归纳为一个注册中心，适配多种结构体 —— 优化的是FieldName的重复动作 */
var cacheV1_1 = map[reflect.Type][]int{}

func optimizeV1_1(u any, age int) error {
	typ := reflect.TypeOf(u)
	val := reflect.ValueOf(u)

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return ErrType
	}
	var index []int
	var ok bool
	if index, ok = cacheV1_1[typ]; !ok {
		fd, exit := typ.FieldByName("Age")
		if !exit {
			return ErrNoField("Age")
		}
		index = fd.Index
		cacheV1_1[typ] = index
	}
	fd := val.FieldByIndex(index)
	if fd.CanSet() {
		fd.SetInt(int64(age))
	}
	return nil
}
