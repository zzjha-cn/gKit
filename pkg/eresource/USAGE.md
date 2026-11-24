# eresource 使用场景与接入指南

根据文件来源和下载权限选择接入方式。业务记录保存资源 ID，需要访问时再按当前身份读取或生成链接。

| 场景 | 接入方式 | 文件经过哪里 |
| --- | --- | --- |
| 定时报表或内部任务产物 | Go 进程直接调用 Manager，本地磁盘与 JSON Store | Go 进程 → 本地磁盘 |
| 登录用户上传合同、工单附件 | 原生 HTTP 接口，OSS 与 MongoDB | 浏览器 → 业务服务 → OSS |
| 浏览器上传大图或视频 | OSS 表单直传，业务服务签发凭证和登记 | 浏览器 → OSS |

## 场景一　本地保存定时报表

适合单进程的内部任务和本地开发。启动时创建一次 `Service`，每次任务通过 `svc.Manager.Upload` 保存文件，进程退出时调用 `svc.Close()`。下面以租户报表 CSV 为例。

```go
import (
    "context"
    "io"

    "github.com/zzjha-cn/gKit/pkg/eresource"
    "github.com/zzjha-cn/gKit/pkg/eresource/config"
    "github.com/zzjha-cn/gKit/pkg/eresource/model"
    "github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

func newReportService(signingSecret string) (*eresource.Service, error) {
    return eresource.New(config.Config{
        Store: config.StoreConfig{
            Type: eresource.StoreTypeJSON, JSONFile: "./data/reports/resources.json",
        },
        Provider: config.ProviderConfig{
            Type: provider.TypeLocal, RootDir: "./data/reports/objects",
            Domain: "localhost:8080", Scheme: "http",
        },
        Access: config.AccessConfig{SignedURLSecret: signingSecret},
    })
}

func saveReport(ctx context.Context, svc *eresource.Service, accountID, runID string, report io.Reader) (string, error) {
    result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
        FileName:       "daily-report.csv",
        Reader:         report,
        Namespace:      "reporting",
        AccountID:      accountID,
        Creator:        "report-job",
        BizType:        "daily-report",
        AccessPolicy:   model.AccessPolicyPrivate,
        IdempotencyKey: "daily-report:" + runID,
    })
    if err != nil {
        return "", err
    }
    return result.Resource.ID, nil
}
```

- `accountID` 和 `runID` 由任务系统提供。同一次任务重试时复用同一个幂等键，并重新提供可读的文件内容。
- 通过 `svc.Manager.Get(ctx, id, accessRequest)` 读取元数据。示例在进程内使用资源 ID。私有上传仍会生成签名地址，因此初始化时需要传入签名密钥。
- 将元数据 JSON 和对象目录放在可持久化的位置。需要多个实例共享资源时，可采用场景二的 OSS 与 MongoDB 配置。

**验证**　同一个租户和任务 ID 连续执行两次，返回的资源 ID 相同。用该 ID 查询能取得文件名、大小和归属信息。

## 场景二　上传和下载租户私有附件

适合合同、发票和工单截图。由业务服务接收文件、执行校验、写入 OSS，并将资源记录写入 MongoDB。前端用浏览器已有的业务会话访问接口。

1. 按 [README 的 OSS 与 MongoDB 配置](README.md#阿里云-oss-与-mongodb) 初始化 `eresource.New`。设置 `Manager.DefaultAccessPolicy = model.AccessPolicyPrivate`、`Access.RequireIdentity = true`，并从密钥配置读取 `Access.SignedURLSecret`。按业务允许的文件大小设置 `Manager.MaxFileSize`。
2. 把 `transport.NewHTTPHandler` 注册到应用的 `http.ServeMux`。用 `transport.WithIdentityResolver` 从 `r.Context()` 的登录会话填入 `Namespace`、`AccountID` 和 `UserID`，并在鉴权失败时返回错误。若附件还受订单或工单权限控制，由业务层在交给资源下载处理前校验相应权限。若该入口只允许私有附件，由业务上传入口固定访问策略，或由中间件校验 `access_policy`。
3. 前端以 `multipart/form-data` 请求 `POST /resource/upload`。表单包含 `file`、`biz_type=invoice` 和 `access_policy=private`。同一次上传重试时复用 `Idempotency-Key` 请求头。成功后将 JSON 响应中的 `data.Resource.id` 写进业务记录。
4. 下载请求走 `GET /resource/{id}/content`，浏览器带业务会话。只有业务允许链接在有效期内被转发时，才向客户端提供 OSS 临时链接。详见下方的[读取权限与安全防护](#读取权限与安全防护)。

**验证**　同租户的授权用户能上传和下载。切换到另一租户后，用相同资源 ID 下载应返回 404。新会话缺少资源租户身份时应返回 401。

## 场景三　浏览器直接上传到 OSS

适合文件上传占用业务服务带宽的场景。在场景二的服务上启用 `Manager.EnableDirectUpload`，保留 `Access.SignedURLSecret`，并根据业务设置 `Manager.MaxFileSize` 与 `Manager.DirectUploadExpire`。在 OSS bucket 的跨域配置中放行实际前端来源对上传地址的 POST 请求。

前端先向业务服务领凭证，再向 OSS 提交签名表单，最后带原会话回业务服务登记。下面假设前端和业务 API 同源，使用现有 Cookie 会话。

```javascript
async function postResource(path, fields) {
  const response = await fetch(path, {
    method: "POST",
    body: new URLSearchParams(fields),
  });
  const result = await response.json();
  if (!response.ok || result.code !== 0) {
    throw new Error(result.message || `资源请求失败（${response.status}）`);
  }
  return result.data;
}

async function uploadDirectly(file) {
  const policy = await postResource("/resource/upload/policy", {
    file_name: file.name,
    content_type: file.type || "application/octet-stream",
    biz_type: "media",
    access_policy: "private",
  });
  if (file.size > policy.max_size) {
    throw new Error("文件超过允许上传的大小");
  }

  const body = new FormData();
  for (const [name, value] of Object.entries(policy.form_fields)) {
    body.append(name, value);
  }
  body.append(policy.file_field, file);
  const uploaded = await fetch(policy.upload_url, { method: policy.method, body });
  if (!uploaded.ok) {
    throw new Error(`OSS 上传失败（${uploaded.status}）`);
  }

  const result = await postResource("/resource/upload/complete", {
    ticket: policy.ticket,
  });
  return result.Resource.id;
}
```

- 将 `form_fields` 原样提交，文件字段放最后。让浏览器生成 multipart 请求的 `Content-Type` 和 boundary。
- 在 `complete` 成功回传资源 ID 后，再写入业务记录。若 OSS 已成功接收文件而登记请求超时，在票据有效期内沿用**同一张票据**重试登记。
- 直传签发时依据声明的文件类型和扩展名校验。业务要求检查实际内容时，可通过 `eresource.WithContentInspector` 注入检视器，登记过程会读取暂存对象并检查。

**验证**　浏览器网络面板应显示凭证和登记请求发往业务服务，文件 POST 发往 OSS。只完成文件 POST 时，资源仍未登记。登记成功后，两次使用同一张有效票据应获得同一个资源 ID。

## 读取权限与安全防护

资源模块以 `Namespace + AccountID` 为租户边界。对非共享资源，按 ID 读取、签发链接和代理下载都会检查租户归属。私有资源还要求调用方有用户身份。同租户的其他登录用户可以读取通过这些检查的私有资源。合同参与人、工单成员、报表订阅人等权限由业务应用判定。选择 `Shared` 会开放跨租户读取，私有附件和租户报表应保持 `Shared=false`。

| 场景 | 读取侧的接入方式 | 应验证的隔离效果 |
| --- | --- | --- |
| 本地报表 | 从任务或会话取得租户身份，元数据走 `Manager.Get`。需要读取内容时先用 `Manager.GetVerified`，再将返回的 `ObjectKey` 交给 `OpenObject`。对象目录只向应用进程开放。 | 租户 A 能读取自己的报表，租户 B 拿到相同 ID 也会被拒绝。 |
| 租户私有附件 | 用登录会话和业务记录检查附件权限，再走 `GET /resource/{id}/content`。OSS bucket 保持私有，附件采用 private ACL。 | 跨租户按 ID 下载返回 404。缺少租户身份且启用 `Access.RequireIdentity` 时返回 401。同租户但无该合同权限的用户也应由业务层拒绝。 |
| OSS 直传 | 凭证只约束本次上传。登记使用领凭证时的同一租户和用户，登记后的文件继续走场景二的读取路径。 | 用户 B 拿到用户 A 的登记票据时返回 403。登记后，跨租户下载仍返回 404。 |

**列表隔离。** HTTP 的 `GET /resource/list` 用会话里的租户覆盖查询范围，`total` 与列表使用同一条件。务必启用 `Access.RequireIdentity`，或由应用在进入路由前拒绝不完整的租户身份。内置 Store 会把空租户查询字段视为未设置过滤条件。Go 侧直接调 `List` 或 `Count` 时，用可信且完整的身份填充查询，例如通过 `manager.ScopedQuery`。按 ID 调用 Manager 时，`nil` 身份保留给受信任的内部任务。向外返回元数据时使用 `resource.View()`，它会清除 bucket、对象 key 和本地路径。

**代理下载和签名链接。** `/content` 每次请求都校验当前身份和资源状态。代理响应带 `Cache-Control: private, no-store`、`nosniff` 和 CSP sandbox，非内联白名单的类型走附件下载。可配置 Referer 白名单和访问频控作为补充。通过模块验签路由下载的本地私有签名链接，在签发时有用户身份才绑定用户，验签时比较用户 ID。OSS 预签名 GET 在有效期内可由持链人使用，业务角色变更或资源软删除都无法让该链接即时失效。

**逐次鉴权的接入姿势。** 合同等敏感文件保持 `Access.PrivateRedirectMinBytes=0`，客户端只拿资源 ID，始终通过业务授权后的代理下载。应用需控制所有可发链接的入口。内置普通上传和直传登记响应会附带 `data.URL`，`/access` 和 `/batch/access` 也会发链接。严格场景由业务封装上传和登记响应，只返回资源 ID，并将发链接路由限制在允许分享的场景。细粒度业务权限也应覆盖列表、详情、下载和修改入口。

## 管理端接入

模块提供资源接口和运维方法，管理页面由业务应用实现。可以从三个场景分别落地。

| 场景 | 管理端页面或操作 | 权限与执行入口 |
| --- | --- | --- |
| 本地报表 | 查看任务产物、下载报表、删除旧文件 | 用操作者有权管理的租户范围调用 Manager。定时清理任务按租户执行。 |
| 私有附件 | 分页查看附件、修改标签或名称、单条或批量删除 | 使用 `GET /resource/list`、`PATCH /resource/{id}`、`DELETE /resource/{id}` 和 `POST /resource/batch/delete`。下载沿用业务授权和代理读取。 |
| OSS 直传 | 查看已登记资源、核对业务记录；清理上传后长期未登记的对象 | 已登记资源复用附件管理。未登记对象不出现在资源列表，可由受信运维任务做孤儿对账，先通过 `OrphanDryRun` 核对，并设置足够的安全窗口。 |

- **租户管理员**。从已验证的角色构造 `AccessRequest`，填充 `Namespace`、`AccountID`、`UserID` 和 `IsAdmin`。需要限定可代为修改或删除的业务线时，设置 `AdminBizTypes`。空列表表示同租户所有业务线。这个字段约束管理员代为修改、删除的权限，管理页的读取范围仍由业务决定。保持 `Access.AllowCrossCreatorDelete=false`，避免默认放开同租户的跨上传者修改和删除。管理端若向外提供 Cookie 会话写操作，应复用应用已有的 CSRF 防护。
- **日常删除**。开启 `Access.SoftDelete` 后，`DELETE` 将资源标为已删除，常规下载会拒绝，内置列表默认只查 active。内置列表允许用 `status=all` 查询已删除记录，若回收记录只供管理员查看，应在业务入口限制该参数。设置 `Access.PurgeRetentionDays`，可用 `Maintenance.Enable` 与 `Maintenance.PurgeExpiredEveryMinutes` 调度清理。批量删除接受表单里的 `ids`，支持重复字段或逗号分隔。接口逐项鉴权，管理端应展示 `data.items` 和 `data.failed`。模块没有回收站恢复路由。
- **平台运维**。租户导出和清退是 `*manager.DefaultManager` 的进程内方法 `ExportTenant` 和 `PurgeTenant`。由单独授权的后台任务指定完整租户身份。导出得到分页的元数据视图。清退会执行硬删除，确认参数为 `<namespace>/<accountID>`，运维端应记录操作者、租户、实际清理数量和错误。确认字符串用于防止误操作，平台权限由业务应用检查。
- **审计记录**。通过 `Access.AuditLogPath` 或 `eresource.WithAuditSink` 接入 `/content` 的下载审计，并按需要启用 `Access.AuditFailClosed`。管理端的修改、删除、导出和清退记录由业务后台写入操作日志。OSS 直链的实际读取发生在存储侧，应用只知道自己何时发出了链接。
