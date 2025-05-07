# 安装golang操作protobuf的工具库
go get google.golang.org/protobuf/proto

# 安装golang构建grpc服务的框架库
go get google.golang.org/grpc

# go 1.21.11版本安装
# 安装帮助protoc生成golang代码的插件
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.4
# go 1.21.11版本安装
# 安装板状protoc生成golang的grpc代码的插件
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.4.0
