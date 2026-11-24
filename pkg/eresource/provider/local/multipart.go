package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

func (p *LocalProvider) partDir(uploadID string) string {
	return filepath.Join(p.rootDir, provider.MultipartStagingDir, uploadID)
}

func (p *LocalProvider) InitMultipart(ctx context.Context, req *model.UploadRequest) (*model.MultipartInit, error) {
	_ = ctx
	objectKey, err := p.objectKey(req)
	if err != nil {
		return nil, err
	}
	uploadID, err := object.RandomHex(16)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(p.partDir(uploadID), 0o755); err != nil {
		return nil, err
	}
	return &model.MultipartInit{
		ObjectKey: objectKey,
		UploadID:  uploadID,
		Provider:  p.Name(),
		StorePath: filepath.Join(p.rootDir, filepath.FromSlash(objectKey)),
	}, nil
}

func (p *LocalProvider) UploadPart(ctx context.Context, objectKey, uploadID string,
	partNumber int, body io.Reader, size int64) (*model.MultipartPart, error) {
	_, _ = ctx, objectKey
	if err := provider.ValidatePartNumber(partNumber); err != nil {
		return nil, err
	}
	// uploadID 来自验签票据，但它仍然会被拼进路径——按同一套
	// 路径段规则校验，免得一个构造出来的 ID 把分片写到目录树外面去。
	if err := validateUploadID(uploadID); err != nil {
		return nil, err
	}
	dir := p.partDir(uploadID)
	if _, err := os.Stat(dir); err != nil {
		return nil, provider.ErrUploadSessionInvalid
	}
	path := filepath.Join(dir, fmt.Sprintf("%05d.part", partNumber))
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	written, err := io.Copy(file, body)
	if err != nil {
		file.Close()
		return nil, err
	}
	// 与 Upload 一样做一次 Sync：分片可能在机器掉电后还要被接着用，
	// 写进 page cache 不算数。
	if err = file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	_ = size
	return &model.MultipartPart{PartNumber: partNumber, ETag: strconv.FormatInt(written, 10), Size: written}, nil
}

func (p *LocalProvider) CompleteMultipart(ctx context.Context, objectKey, uploadID string,
	parts []model.MultipartPart) (*model.ProviderResult, error) {
	_ = ctx
	if err := validateUploadID(uploadID); err != nil {
		return nil, err
	}
	dir := p.partDir(uploadID)
	if _, err := os.Stat(dir); err != nil {
		return nil, provider.ErrUploadSessionInvalid
	}
	// 按分片序号排序再拼：客户端完全可能乱序上传，也可能乱序提交清单。
	ordered := append([]model.MultipartPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PartNumber < ordered[j].PartNumber })

	fullPath := filepath.Join(p.rootDir, filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return nil, err
	}
	// 先写临时文件再链接到最终路径：拼到一半崩溃不能留下一个半截的"正式"对象。
	tmp, err := os.CreateTemp(filepath.Dir(fullPath), ".merge-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	var total int64
	for _, part := range ordered {
		partPath := filepath.Join(dir, fmt.Sprintf("%05d.part", part.PartNumber))
		src, openErr := os.Open(partPath)
		if openErr != nil {
			tmp.Close()
			return nil, fmt.Errorf("%w: part %d", provider.ErrUploadSessionInvalid, part.PartNumber)
		}
		n, copyErr := io.Copy(tmp, src)
		src.Close()
		if copyErr != nil {
			tmp.Close()
			return nil, copyErr
		}
		total += n
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	// Publish once so concurrent completions cannot replace an inspected object.
	if err = os.Link(tmpName, fullPath); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(dir)
	return &model.ProviderResult{
		Provider:  p.Name(),
		ObjectKey: objectKey,
		StorePath: fullPath,
		Size:      total,
	}, nil
}

func (p *LocalProvider) AbortMultipart(ctx context.Context, objectKey, uploadID string) error {
	_, _ = ctx, objectKey
	if err := validateUploadID(uploadID); err != nil {
		return err
	}
	return os.RemoveAll(p.partDir(uploadID))
}

// validateUploadID 把 uploadID 限制成十六进制串。
//
// 它会被拼进文件路径，哪怕来源是自己签发的票据也要校验——
// 「这个值可信」的前提是签名密钥没泄露，而路径穿越的代价太大，
// 不值得把它押在另一个假设上。
func validateUploadID(uploadID string) error {
	if uploadID == "" || len(uploadID) > 128 {
		return provider.ErrUploadSessionInvalid
	}
	for _, r := range uploadID {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-', r == '_':
		default:
			return provider.ErrUploadSessionInvalid
		}
	}
	return nil
}
