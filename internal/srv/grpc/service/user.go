package service

import (
	"context"

	"github.com/zzjha-cn/gKit/internal/srv/grpc/gen"
)

// 实际的业务逻辑(核心与grpc无关)

type UserSrv struct {
	// base.RpcServer

	// grpc已经生成了对应的方法,组合对象并复写方法就可以
	gen.UnimplementedUserServiceServer
}

func (u *UserSrv) GetUser(ctx context.Context, req *gen.GetByIdReq) (*gen.GetByIdResp, error) {
	resp := &gen.GetByIdResp{}
	user := &gen.User{}

	user.Id = req.Id
	user.Name = "demo"

	resp.User = user

	return resp, nil
}

func (u *UserSrv) Start() error {
	// 在服务准备完成后--预热了缓存等等
	// 调用启动方法
	// 将服务注册到rpc的注册中心

	gen.RegisterUserServiceServer(nil, u)

	return nil
}
