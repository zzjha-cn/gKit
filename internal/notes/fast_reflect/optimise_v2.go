package fast_refelct

import (
	"reflect"
	"unsafe"
)

/* 在cache缓存的基础上，还可以直接使用unsafe的字段偏移量来完成值的修改 —— 优化的是setInt动作的冗余保证*/
var cacheV2 = map[reflect.Type]uintptr{}

// 空接口实际上是具有两个指针的结构的语法糖：第一个指向有关类型的信息，第二个指向值
// 可以使用结构体中字段偏移量来直接寻址该值的字段
type intfaceMark struct {
	typ   unsafe.Pointer
	value unsafe.Pointer
}

func optimizeV2(u any, age int) error {
	typ := reflect.TypeOf(u)
	val := reflect.ValueOf(u)

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return ErrType
	}

	ptr, ok := cacheV2[typ]
	if !ok {
		structField, exit := typ.FieldByName("Age")
		if !exit {
			return ErrNoField("Age")
		}
		ptr = structField.Offset
		cacheV2[typ] = ptr
	}

	structPtr := (*intfaceMark)(unsafe.Pointer(&u)).value
	*(*int)(unsafe.Pointer(uintptr(structPtr) + ptr)) = age
	return nil
}
