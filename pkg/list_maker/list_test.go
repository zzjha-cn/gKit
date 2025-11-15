package listmaker

import (
	"fmt"
	"testing"
)

func TestListMakerCommon(t *testing.T) {
	var a = 0
	CommonRecursively([]func() error{
		func() error {
			a++
			fmt.Println(a)
			return nil
		},
		func() error {
			a--
			fmt.Println(a)
			return nil
		},
		func() error {
			a = a + 2
			fmt.Println(a)
			return nil
		},
	})

	if a != 2 {
		t.Errorf("fail a = %d", a)
	}
}

type ut struct {
	cnt int
}

func (u *ut) Exec(ctx *Context) bool {
	u.cnt++
	fmt.Println(u.cnt)
	ctx.Result = u.cnt
	return false
}

func TestMakeList2(t *testing.T) {
	u := &ut{}
	ctx := &Context{}
	RecursivelyWithImpl([]IList2Recursive{u, u, u, u, u}, ctx)

	fmt.Println(ctx.Result)
}
