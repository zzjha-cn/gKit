package test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzjha-cn/gKit/internal/srv/grpc/base"
	"github.com/zzjha-cn/gKit/internal/srv/grpc/gen"
	"github.com/zzjha-cn/gKit/internal/srv/grpc/service"
)

func TestUserRpcServerLocal(t *testing.T) {
	srv := base.NewRpcServer()

	listener, err := net.Listen("tcp", "127.0.0.1:8881")
	require.Nil(t, err)

	srv.Server.RegisterService(&gen.UserService_ServiceDesc, &service.UserSrv{})
	srv.Serve(listener)
}

func TestUserRpcClientLocal(t *testing.T) {
	cli, _ := base.NewRpcClient("127.0.0.1:8881")
	userClient := gen.NewUserServiceClient(cli)
	resp, err := userClient.GetUser(context.Background(), &gen.GetByIdReq{Id: 123})
	require.Nil(t, err)
	assert.Equal(t, "demo", resp.User.Name)
}
