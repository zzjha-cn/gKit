package transport

import (
	"testing"
)

// parseByteRange 的边界：HTTP 允许服务端忽略 Range，因此只有
// 「语法合法但区间整个落在对象之外」才是错误，其余一律退回整体响应。
func TestParseByteRange(t *testing.T) {
	const size = 10
	cases := map[string]struct {
		header         string
		size           int64
		wantOffset     int64
		wantLength     int64
		wantRanged     bool
		wantUnsatisfia bool
	}{
		"闭区间":      {"bytes=2-5", size, 2, 4, true, false},
		"开区间":      {"bytes=5-", size, 5, 5, true, false},
		"后缀区间":     {"bytes=-3", size, 7, 3, true, false},
		"后缀超过对象":   {"bytes=-99", size, 0, 10, true, false},
		"末尾超过对象":   {"bytes=8-99", size, 8, 2, true, false},
		"整个对象":     {"bytes=0-9", size, 0, 10, true, false},
		"起点越界":     {"bytes=10-12", size, 0, 0, false, true},
		"没有 Range": {"", size, 0, 0, false, false},
		"单位不是字节":   {"items=0-1", size, 0, 0, false, false},
		"多区间":      {"bytes=0-1,4-5", size, 0, 0, false, false},
		"倒置区间":     {"bytes=5-2", size, 0, 0, false, false},
		"非数字":      {"bytes=abc", size, 0, 0, false, false},
		"缺少连字符":    {"bytes=5", size, 0, 0, false, false},
		"负起点":      {"bytes=-0", size, 0, 0, false, false},
		// 大小未知时既换算不了后缀区间，也判定不了可满足性，一律退回整体响应。
		"对象大小未知": {"bytes=0-5", 0, 0, 0, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ranged, err := parseByteRange(tc.header, tc.size)
			if tc.wantUnsatisfia {
				if err == nil {
					t.Fatalf("expected errRangeNotSatisfiable, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ranged != tc.wantRanged {
				t.Fatalf("ranged = %t, want %t", ranged, tc.wantRanged)
			}
			if !ranged {
				return
			}
			if got.offset != tc.wantOffset || got.length != tc.wantLength {
				t.Errorf("range = {%d,%d}, want {%d,%d}",
					got.offset, got.length, tc.wantOffset, tc.wantLength)
			}
		})
	}
}
