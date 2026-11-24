package local

import (
	"context"
	"io"
	"os"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// OpenRange 按字节区间读取本地文件。
func (p *LocalProvider) OpenRange(ctx context.Context, objectKey string, offset, length int64) (io.ReadCloser, *model.ObjectStat, error) {
	_ = ctx
	storePath, err := p.resolve(objectKey)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(storePath)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	stat := &model.ObjectStat{Exists: true, Size: info.Size(), UpdatedAt: info.ModTime()}
	if offset > 0 {
		if _, err = file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, nil, err
		}
	}
	if length <= 0 {
		return file, stat, nil
	}
	// LimitReader 不带 Close，这里套一层把关闭责任还给文件本身，
	// 否则每一次区间下载都会泄漏一个文件描述符。
	return &limitedFile{Reader: io.LimitReader(file, length), closer: file}, stat, nil
}

// limitedFile 把一个受限读流与它底层的关闭方法绑在一起。
type limitedFile struct {
	io.Reader
	closer io.Closer
}

func (f *limitedFile) Close() error { return f.closer.Close() }
