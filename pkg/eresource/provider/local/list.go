package local

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

// ListObjects 遍历本地存储根目录。
//
// 每次调用都重新走一遍目录树再按 key 排序取一页。这是 O(n) 的，
// 比 OSS 那种天然有序的列举笨得多——但本地 provider 的定位是开发与小规模部署，
// 为它维护一棵常驻索引不划算。真正的量级场景跑在对象存储上。
func (p *LocalProvider) ListObjects(ctx context.Context, prefix, marker string, limit int) ([]model.ObjectEntry, string, error) {
	limit = object.NormalizeListLimit(limit)
	var all []model.ObjectEntry
	err := filepath.WalkDir(p.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个目录读不动不该让整次对账失败：对账是巡检，
			// 跳过一棵子树的代价是这一批少发现几个孤儿，下次再发现。
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(p.rootDir, path)
		if relErr != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		all = append(all, model.ObjectEntry{Key: key, Size: info.Size(), UpdatedAt: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	// 按 key 排序，分页才稳定——否则同一个对象可能被翻到两次或一次都翻不到。
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })

	start := 0
	if marker != "" {
		start = sort.Search(len(all), func(i int) bool { return all[i].Key > marker })
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]
	next := ""
	if end < len(all) && len(page) > 0 {
		next = page[len(page)-1].Key
	}
	return page, next, nil
}
