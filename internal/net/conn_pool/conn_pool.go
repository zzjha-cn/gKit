package connpool

import (
	"fmt"
	"net"
	"time"

	"github.com/silenceper/pool"
)

type Pool struct {
	data pool.Pool
}

func NewConnectionPool(addr string) (*Pool, error) {
	p, err := pool.NewChannelPool(&pool.Config{
		InitialCap: 10,
		MaxCap:     100,
		MaxIdle:    50,
		Factory: func() (interface{}, error) {
			return net.Dial("tcp", addr)
		},
		IdleTimeout: time.Minute,
		Close: func(i interface{}) error {
			return i.(net.Conn).Close()
		},
	})
	if nil != err {
		return nil, err
	}
	return &Pool{
		data: p,
	}, nil
}

func (p *Pool) Ping(bys []byte) error {
	obj, err := p.data.Get()
	if err != nil {
		return err
	}
	conn, _ := obj.(net.Conn)
	_, err = conn.Write(bys)
	if err != nil {
		return err
	}

	resp := make([]byte, 4)
	_, err = conn.Read(resp)
	if err != nil {
		return err
	}

	if string(resp) != "pong" {
		return fmt.Errorf("no expect response")
	}
	return nil
}
