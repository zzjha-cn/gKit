package base

import (
	"google.golang.org/grpc"
)

type RpcServer struct {
	*grpc.Server
}

func NewRpcServer() *RpcServer {
	res := &RpcServer{}
	res.Server = grpc.NewServer()
	return res
}
