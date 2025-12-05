package tools

import (
	"errors"
	"reflect"
	"strings"
	"time"
)

func GetFieldNameMap(data any, tagTyp string) (map[string]string, error) {
	var (
		record = map[string]string{}
	)

	typ := reflect.TypeOf(data)
	val := reflect.ValueOf(data)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil, errors.New("invalid type(must struct)")
	}

	for i := 0; i < typ.NumField(); i++ {
		fd := typ.Field(i)

		if fd.Type.Kind() == reflect.Pointer ||
			fd.Type.Kind() == reflect.Struct {
			if fd.Type == reflect.TypeOf(time.Time{}) {
				tagVal, ok := GetTagString(fd, tagTyp)
				if ok {
					record[tagVal] = fd.Name
				}
				continue
			}
			fdVal := val.Field(i)
			if fd.Type.Kind() == reflect.Pointer {
				fdVal = fdVal.Elem()
			}
			if subMap, err := GetFieldNameMap(fdVal.Interface(), tagTyp); err == nil {
				for k, v := range subMap {
					record[k] = v
				}
			}
		} else {
			tagVal, ok := GetTagString(fd, tagTyp)
			if ok {
				record[tagVal] = fd.Name
			}
		}
	}
	return record, nil
}

func GetTagString(structTag reflect.StructField, tag string) (string, bool) {
	strcutTag := structTag.Tag
	tagValStr := strcutTag.Get(tag)
	tagValArr := strings.Split(tagValStr, ",")
	if len(tagValArr) == 0 {
		return "", false
	}
	tagVal := tagValArr[0]
	if len(tagVal) == 0 || tagVal == "-" {
		return "", false
	}
	return tagValArr[0], true
}
