package manager

import (
	"context"
	"errors"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// AuditEvent 一条审计记录：谁、在什么时候、对哪个资源做了什么。
type AuditEvent struct {
	At         time.Time `json:"at"`
	Action     string    `json:"action"` // 见 AuditAction* 常量
	ResourceID string    `json:"resource_id"`
	ObjectKey  string    `json:"object_key"`
	Namespace  string    `json:"namespace"`
	AccountID  string    `json:"account_id"`
	UserID     string    `json:"user_id"`
	ClientIP   string    `json:"client_ip"`
	UserAgent  string    `json:"user_agent"`
	Bytes      int64     `json:"bytes"`
	Route      string    `json:"route"` // 下载走的路径：proxy / signed / public
}

// 审计动作。
const (
	AuditActionDownload = "download"
	AuditActionUpload   = "upload"
	AuditActionDelete   = "delete"
	AuditActionUpdate   = "update"
)

// AuditSink 接收审计事件。
//
// 与 Metrics 分开，是因为两者的失败语义完全相反：
// 指标丢一条无所谓，审计丢一条就是合规事故。所以这个接口**返回 error**，
// 而 Metrics 不返回——调用方必须明确表态「这条没收下」。
//
// 实现应当是异步的（写进缓冲、由后台落盘或发远端），但缓冲满时怎么办
// 不该由实现自己决定：见 ErrAuditRejected 与 access.audit_fail_closed。
type AuditSink interface {
	Record(ctx context.Context, event *AuditEvent) error
}

// ErrAuditRejected 审计未能收下这条事件（典型是缓冲已满）。
//
// 这是整个审计设计里唯一需要配置参与的决策点：
//   - 合规场景 fail-closed：收不下就拒绝本次访问，宁可不服务，不可无记录；
//   - 非合规场景：丢弃并告警，访问照常。
//
// 两种行为差别巨大，不能是实现里的一行默认值。
var ErrAuditRejected = errors.New("eresource: audit sink rejected the event")

// nopAudit 默认实现：不记录，也永远不拒绝。
type nopAudit struct{}

func (nopAudit) Record(context.Context, *AuditEvent) error { return nil }

// SetAuditSink 替换审计实现。约定在启动期调用。
func (m *DefaultManager) SetAuditSink(sink AuditSink) {
	if sink == nil {
		sink = nopAudit{}
	}
	m.audit.Store(&sink)
}

func (m *DefaultManager) auditSink() AuditSink {
	stored, _ := m.audit.Load().(*AuditSink)
	if stored == nil {
		return nopAudit{}
	}
	return *stored
}

// Audit 记录一条审计事件。
//
// 返回的 error 只有在 access.audit_fail_closed 打开时才非 nil——
// 关闭时审计失败被降级成「丢了一条」，调用方不必在每个路径上写两套分支。
// 这个开关的语义必须集中在这里，散到各调用点迟早会漏掉一处。
func (m *DefaultManager) Audit(ctx context.Context, event *AuditEvent) error {
	if event == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	err := m.auditSink().Record(ctx, event)
	if err == nil {
		return nil
	}
	if m.config.Access.AuditFailClosed {
		return err
	}
	return nil
}

// AuditDownload 组装并记录一次下载审计。
func (m *DefaultManager) AuditDownload(ctx context.Context, resource *model.Resource,
	req *model.AccessRequest, route string, bytes int64) error {
	event := &AuditEvent{
		Action: AuditActionDownload,
		Route:  route,
		Bytes:  bytes,
	}
	if resource != nil {
		event.ResourceID = resource.ID
		event.ObjectKey = resource.ObjectKey
		event.Namespace = resource.Namespace
		event.AccountID = resource.AccountID
	}
	if req != nil {
		event.UserID = req.UserID
		event.ClientIP = req.ClientIP
		event.UserAgent = req.UserAgent
	}
	return m.Audit(ctx, event)
}
