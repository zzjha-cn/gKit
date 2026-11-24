package manager

import (
	"context"
	"errors"
	"io"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// ContentInspector synchronously approves content before it becomes accessible.
// Resource contains the request metadata and may not yet exist in the store.
type ContentInspector interface {
	Inspect(ctx context.Context, resource *model.Resource, body []byte) error
}

// ErrContentRejected 内容未通过检视。
var ErrContentRejected = errors.New("eresource: content rejected by inspector")

// SetContentInspector 注入内容检视实现。约定在启动期调用。
func (m *DefaultManager) SetContentInspector(inspector ContentInspector) {
	if inspector == nil {
		m.inspector.Store((*ContentInspector)(nil))
		return
	}
	m.inspector.Store(&inspector)
}

func (m *DefaultManager) contentInspector() ContentInspector {
	stored, _ := m.inspector.Load().(*ContentInspector)
	if stored == nil {
		return nil
	}
	return *stored
}

func (m *DefaultManager) inspectContent(ctx context.Context, resource *model.Resource, body []byte) error {
	if inspector := m.contentInspector(); inspector != nil {
		return inspector.Inspect(ctx, resource, body)
	}
	return nil
}

func (m *DefaultManager) readAndInspectObject(ctx context.Context, resource *model.Resource, maxSize int64) ([]byte, error) {
	reader, _, err := m.OpenObject(ctx, resource.ObjectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != resource.Size {
		return nil, errors.New("eresource: stored object size changed before inspection")
	}
	if err = m.validateMimeConsistency(resource.MimeType, body); err != nil {
		return nil, err
	}
	if err = m.validatePublicContent(resource.AccessPolicy, resource.MimeType, resource.Ext, body); err != nil {
		return nil, err
	}
	if err = m.inspectContent(ctx, resource, body); err != nil {
		return nil, err
	}
	return body, nil
}

// ErrPublicUploadRequiresInspection rejects public tickets issued before private staging was enabled.
var ErrPublicUploadRequiresInspection = errors.New("eresource: request a new upload ticket before publishing this object")
