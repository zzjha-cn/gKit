# eresource

`github.com/zzjha-cn/gKit/pkg/eresource` 管理文件内容、元数据和访问权限。
内置阿里云 OSS 和本地磁盘 Provider，以及 MongoDB 和 JSON 元数据 Store。
常规能力包括上传、更新、查询、删除、签名下载和 Range 下载。直传、分片、
去重、租户配额、审计、CDN 和生命周期维护按配置启用。

参见 [三个使用场景与接入指南](USAGE.md)，了解本地任务产物、私有附件和 OSS 直传的接入方式。

## 目录

| 目录 | 职责 |
| --- | --- |
| `eresource.go` | 模块装配和生命周期，应用通过 `eresource.New` 接入 |
| `config/`、`model/` | 配置和资源模型 |
| `manager/` | 上传、鉴权、签名、删除和维护等业务流程 |
| `transport/` | 原生 HTTP 路由、身份解析和请求响应 |
| `provider/` | 对象存储接口和可选能力 |
| `provider/local/`、`provider/aliyunoss/` | 本地磁盘和阿里云 OSS 实现 |
| `store/` | 元数据、用量持久化接口 |
| `store/jsonstore/`、`store/mongostore/` | JSON 和 MongoDB 实现 |
| `cdn/` | CDN 接口、阿里云客户端和边缘访问日志解析 |
| `integration/` | 按业务功能组织的跨层测试 |

## 本地接入

下面的函数把资源接口注册到业务已有的 `http.ServeMux`。
`resolve` 由调用方实现，用鉴权中间件的会话填充
`model.AccessRequest` 的 `Namespace`、`AccountID` 和 `UserID`。
在 HTTP 服务停止接收请求并完成现有请求后调用返回的清理函数。

```go
import (
    "net/http"

    "github.com/zzjha-cn/gKit/pkg/eresource"
    "github.com/zzjha-cn/gKit/pkg/eresource/config"
    "github.com/zzjha-cn/gKit/pkg/eresource/model"
    "github.com/zzjha-cn/gKit/pkg/eresource/provider"
    "github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

func mountResources(mux *http.ServeMux, resolve transport.IdentityResolver, signingSecret string) (func(), error) {
    svc, err := eresource.New(config.Config{
        Store: config.StoreConfig{
            Type: eresource.StoreTypeJSON, JSONFile: "./data/resources.json",
        },
        Provider: config.ProviderConfig{
            Type: provider.TypeLocal, RootDir: "./data/objects",
            Domain: "localhost:8080", Scheme: "http",
        },
        Manager: config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPrivate},
        Access: config.AccessConfig{
            RequireIdentity: true, SignedURLSecret: signingSecret,
        },
    })
    if err != nil {
        return nil, err
    }
    handler, err := transport.NewHTTPHandler(
        svc.Manager, svc.Provider, svc.Config, transport.WithIdentityResolver(resolve),
    )
    if err != nil {
        svc.Close()
        return nil, err
    }
    handler.Register(mux)
    return func() {
        handler.Close()
        svc.Close()
    }, nil
}
```

JSON Store 适合本地开发。私有文件通过 `GET /resource/{id}/content` 下载，
请勿将 `RootDir` 挂为静态目录。私有资源上传会签发访问链接，
`signingSecret` 由应用从密钥配置读取。将 `nil` 传给 `WithIdentityResolver` 会使用默认请求头解析器，
网关需剥除客户端身份头，并根据会话覆写。

不需要 HTTP 时，直接调用 `svc.Manager.Upload(ctx, req)` 等方法。
`eresource.WithStore` 和 `eresource.WithProvider` 可注入自定义实现。
直接构造内置实现时，从对应目录导入 `local.NewLocalProvider`、
`aliyunoss.NewAliOSSProvider`、`jsonstore.NewJSONStore`、
`mongostore.NewMongoStore` 或 `cdn.NewAliCDNRefresher`。
按配置创建对象存储也可以使用 `eresource.NewProvider`。

## 阿里云 OSS 与 MongoDB

- 将 `Provider.Type` 设置为 `provider.TypeAliyunSS`，配置 `Bucket`、`Endpoint`、
  `AccessKey`、`SecretKey`。地域可通过 `Region` 显式指定。
  从应用密钥配置读取凭据。
- 将 `Store.Type` 设置为 `eresource.StoreTypeMongo`，配置 `MongoDB`、`MongoCollection`
  和 `MongoURI`。应用也可通过 `eresource.WithMongoClient(client)` 复用官方驱动的
  `*mongo.Client`。模块启动时确保索引，关闭时只释放自己创建的连接。
- 从旧模块切换时，沿用现有 bucket、对象前缀、元数据位置和签名配置。
  如果已有 `*emongo.Client`，把它嵌入的 `Client` 字段传给 `WithMongoClient`。
  旧的身份解析器改为接收 `*http.Request`，从 `r.Context()` 读取会话。

## HTTP 路由

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/resource/upload` | multipart 表单上传，文件字段为 `file` |
| GET | `/resource/list`、`/resource/{id}` | 查询列表和元数据 |
| PATCH、DELETE | `/resource/{id}` | 更新元数据和删除资源 |
| GET | `/resource/{id}/access` | 取得访问链接 |
| GET、HEAD | `/resource/{id}/content` | 受控下载和大小探测 |
| GET、HEAD | `/resource/signed/{objectKey...}` | 默认签名下载路径 |
| POST | `/resource/batch/access`、`/resource/batch/delete` | 批量签发和删除 |
| POST | `/resource/upload/policy`、`/resource/upload/complete` | OSS 直传凭证和登记 |
| POST | `/resource/multipart/{init,part,complete,abort}` | 分片上传四个阶段 |
| POST | `/resource/proxy-upload` | 远端抓取中转 |

最后三类路由按配置开放。签名下载路径可通过 `Provider.SignedBasePath` 修改。

## 验证

```sh
go test ./pkg/eresource/...
```

真实 OSS 联调需要专用测试 bucket。设置 `ERESOURCE_OSS_BUCKET`、
`ERESOURCE_OSS_ENDPOINT`、`ERESOURCE_OSS_AK`、`ERESOURCE_OSS_SK`
后显式执行下列命令。测试会在 bucket 创建并清理测试对象。

```sh
go test -tags eresource_oss_integration -count=1 -run '^TestAliOSSManual' ./pkg/eresource/provider/aliyunoss
```
