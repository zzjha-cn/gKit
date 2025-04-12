package fast_refelct

import (
	"reflect"
	"unsafe"
)

/* 使用描述符 -- 将功能与结构拆分(不使用cache的话，会更快，但是不能扩展) */

// 优化后的处理会比较不同

var cahceV4 = map[uintptr]DescType{}

func optimizeV4(u any, age int) error {
	var ttt DescType
	var ok bool
	intMark := (*intfaceMark)(unsafe.Pointer(&u))
	if ttt, ok = cahceV4[uintptr(intMark.typ)]; !ok {
		t, err := DescribeType(u)
		if err != nil {
			return err
		}
		cahceV4[uintptr(intMark.typ)] = t
		ttt = t
	}
	ProcessStructAge(u, ttt, age)
	return nil
}

type DescType uintptr

// 获取属性描述符
func DescribeType(u any) (DescType, error) {
	typ := reflect.TypeOf(u)

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return 0, ErrType
	}

	fd, exit := typ.FieldByName("Age")
	if !exit {
		return 0, ErrNoField("Age")
	}
	offset := fd.Offset
	return DescType(offset), nil
}

// 通过描述符改变值
func ProcessStructAge(u any, ti DescType, age int) error {
	structPtr := (*intfaceMark)(unsafe.Pointer(&u)).value
	*(*int)(unsafe.Pointer(uintptr(structPtr) + uintptr(ti))) = age
	return nil
}
