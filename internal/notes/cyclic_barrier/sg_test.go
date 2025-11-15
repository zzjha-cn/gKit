package cyclic_barrier

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

// singlefight的使用

func TestSg(t *testing.T) {
	var sg = singleflight.Group{}
	var key = "request_key"
	var wg = sync.WaitGroup{}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sg.Do(key, func() (interface{}, error) {
				time.Sleep(1 * time.Second)
				fmt.Println("call the funcion")
				return nil, nil
			},
			)
		}()
	}

	wg.Wait()
}
