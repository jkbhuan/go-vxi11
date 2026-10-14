package main

import (
	"strings"
	"testing"
)

func TestParseDefiniteLengthBlock(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"单字节长度", "#15hello", "hello", false},
		{"两位长度", "#210abcdefghij", "abcdefghij", false},
		{"数据比声明长（尾部可忽略）", "#15helloXXXX", "hello", false},
		{"不是定长块", "hello", "", true},
		{"长度位数非法", "#0hello", "", true},
		{"长度字段不完整", "#2", "", true},
		{"长度字段非数字", "#1xabcd", "", true},
		{"数据不足", "#19short", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDefiniteLengthBlock([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("解析结果 = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestDecodeSTB(t *testing.T) {
	got := decodeSTB(0x10)
	if len(got) != 1 || !strings.Contains(got[0], "MAV") {
		t.Errorf("0x10 解析为 %v, 期望只含 MAV", got)
	}
	if got := decodeSTB(0); len(got) != 1 || got[0] != "无位被置" {
		t.Errorf("0x00 解析为 %v", got)
	}
	if got := decodeSTB(0x50); len(got) != 2 {
		t.Errorf("0x50 解析为 %v, 期望 2 位", got)
	}
}
