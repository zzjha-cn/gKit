package fast_refelct

import (
	"errors"
	"fmt"
	"reflect"
)

type User struct {
	Name string
	Age  int
	G    Geo
}

type Geo struct {
	X int
	Y int
	S string
}

var (
	ErrType    = errors.New("类型错误需要结构体或者结构体指针类型")
	ErrNoField = func(fd string) error { return fmt.Errorf("没有对应属性, %s", fd) }
)

func SetAge(u any, a int) error {
	typ := reflect.TypeOf(u)
	val := reflect.ValueOf(u)

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return ErrType
	}

	fd := val.FieldByName("Age")
	if fd.CanSet() {
		fd.SetInt(int64(a))
	}
	return nil
}

func SetStdAge(u any, age int) error {
	if user, ok := u.(*User); ok {
		user.Age = age
	}
	return nil
}
