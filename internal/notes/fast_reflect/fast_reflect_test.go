package fast_refelct

import (
	"testing"
)

func TestSetAge(t *testing.T) {
	tests := []struct {
		name string
		u    any
		age  int

		wantErr   bool
		ErrString string
	}{
		{
			name: "功能测试",
			u: &User{ // bugfix: 使用非指针的话，会导致reflect.CanSet返回false
				Age: 1,
			},
			age:     111,
			wantErr: false,
		},
		{
			name:    "传入非法",
			u:       1,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := optimizeV4(tt.u, tt.age); (err != nil) != tt.wantErr {
				t.Errorf("SetAge() error = %v, wantErr %v", err, tt.wantErr)
			}
			if user, ok := tt.u.(*User); ok && user.Age != tt.age {
				t.Fatal("修改失败")
			}
		})
	}
}

func BenchmarkSimple(b *testing.B) {
	type run struct {
		age int
		ff  string
	}
	runs := []run{
		{111, "std_setValue"},
		{111, "base"},
		{22, "op_cache_v1"},
		{33, "op_unsafe_v2"},
		{44, "op_mapKey_v3"},
		{55, "op_describe_v4"},
	}
	for _, r := range runs {
		var part string
		var f func(u any, age int) error
		switch r.ff {
		case "std_setValue":
			part = "std_setValue"
			f = SetStdAge
		case "base":
			part = "base_simple"
			f = SetAge
		case "op_cache_v1":
			part = "op_cache_v1"
			f = optimizeV1_1
		case "op_unsafe_v2":
			part = "op_unsafe_v2"
			f = optimizeV2
		case "op_mapKey_v3":
			part = "op_mapKey_v3"
			f = optimizeV3
		case "op_describe_v4":
			part = "op_describe_v4"
			f = optimizeV4
		default:
			continue
		}

		b.Run(part, func(b *testing.B) {
			b.ReportAllocs()
			var err error
			var u = &User{}
			for i := 0; i < b.N; i++ {
				err = f(u, r.age)
				if nil != err {
					b.Fatal("执行失败", err)
					return
				}
			}
			b.StopTimer()
		})
	}
}

// BenchmarkSimple/std_setValue-8         	659803609	         1.813 ns/op	       0 B/op	       0 allocs/op
// BenchmarkSimple/base_simple-8          	14242274	        83.28 ns/op	       8 B/op	       1 allocs/op
// BenchmarkSimple/op_cache_v1-8          	38810584	        31.27 ns/op	       0 B/op	       0 allocs/op
// BenchmarkSimple/op_unsafe_v2-8         	49728358	        24.80 ns/op	       0 B/op	       0 allocs/op
// BenchmarkSimple/op_mapKey_v3-8         	224674586	         5.415 ns/op	       0 B/op	       0 allocs/op
// BenchmarkSimple/op_describe_v4-8       	217720764	         5.453 ns/op	       0 B/op	       0 allocs/op
