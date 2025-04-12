package fast_refelct

import (
	"reflect"
	"unsafe"
)

/* 针对cache的进一步优化，提高map的索引速度——使用更加简单的key */

var cacheUnsafeV3 = map[uintptr]uintptr{}

func optimizeV3(u any, age int) error {
	infMark := (*intfaceMark)(unsafe.Pointer(&u))

	offset, ok := cacheUnsafeV3[uintptr(infMark.typ)]
	if !ok {
		typ := reflect.TypeOf(u)
		val := reflect.ValueOf(u)

		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
			val = val.Elem()
		}

		if typ.Kind() != reflect.Struct {
			return ErrType
		}

		fd, exit := typ.FieldByName("Age")
		if !exit {
			return ErrNoField("Age")
		}
		offset = fd.Offset
		cacheUnsafeV3[uintptr(infMark.typ)] = offset
	}
	structPtr := infMark.value
	*(*int)(unsafe.Pointer(uintptr(structPtr) + offset)) = age
	return nil
}
