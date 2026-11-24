package local

import (
	"context"
	"os"
	"path/filepath"
)

// PutObject 按指定 key 原样写入本地盘。
//
// 复用 Upload 那套「写临时文件 → Sync → Rename」的流程：
// 副本同步写到一半崩溃，不能留下一个大小对、内容截断的"副本"——
// 那比没有副本更危险，因为 VerifyReplica 按大小核对会认为它是好的。
func (p *LocalProvider) PutObject(ctx context.Context, objectKey string, content []byte, contentType, accessPolicy string) error {
	_, _, _ = ctx, contentType, accessPolicy
	// 复用 resolve 做路径穿越校验：objectKey 来自主存储的元数据，
	// 但副本同步的源不一定永远可信，这道校验没有理由省。
	fullPath, err := p.resolve(objectKey)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(fullPath), ".replica-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err = tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, fullPath)
}
