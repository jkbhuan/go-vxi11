package vxi11

import (
	"bytes"
	"errors"
	"testing"
)

func TestXDRRoundTrip(t *testing.T) {
	e := &xdrEncoder{}
	e.int32(-1)
	e.uint32(0xDEADBEEF)
	e.uint16(0x1234)
	e.char('\n')
	e.boolean(true)
	e.boolean(false)
	e.opaque([]byte{1, 2, 3}) // 需要补 1 个零字节
	e.string("inst0")         // 需要补 3 个零字节
	e.opaque(nil)             // 空变长数据
	e.string("")              // 空字符串
	encoded := e.bytes()

	d := newXDRDecoder(encoded)
	if got := d.int32(); got != -1 {
		t.Errorf("int32 = %d", got)
	}
	if got := d.uint32(); got != 0xDEADBEEF {
		t.Errorf("uint32 = %#x", got)
	}
	if got := d.uint16(); got != 0x1234 {
		t.Errorf("uint16 = %#x", got)
	}
	if got := d.char(); got != '\n' {
		t.Errorf("char = %q", got)
	}
	if !d.boolean() {
		t.Error("boolean(true) 解码为 false")
	}
	if d.boolean() {
		t.Error("boolean(false) 解码为 true")
	}
	if got := d.opaque(16); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("opaque = %v", got)
	}
	if got := d.string(16); got != "inst0" {
		t.Errorf("string = %q", got)
	}
	if got := d.opaque(16); len(got) != 0 {
		t.Errorf("空 opaque = %v", got)
	}
	if got := d.string(16); got != "" {
		t.Errorf("空 string = %q", got)
	}
	if err := d.error(); err != nil {
		t.Fatalf("解码出错: %v", err)
	}
	if d.remaining() != 0 {
		t.Errorf("剩余 %d 字节未消费", d.remaining())
	}
}

// TestXDRAlignment 检查变长数据的 4 字节对齐补零。
func TestXDRAlignment(t *testing.T) {
	e := &xdrEncoder{}
	e.opaque([]byte("abc"))
	got := e.bytes()
	want := []byte{0, 0, 0, 3, 'a', 'b', 'c', 0}
	if !bytes.Equal(got, want) {
		t.Errorf("编码 = %v, 期望 %v", got, want)
	}

	// 长度正好对齐时不应有多余的补零。
	e = &xdrEncoder{}
	e.opaque([]byte("abcd"))
	if got, want := len(e.bytes()), 8; got != want {
		t.Errorf("长度 = %d, 期望 %d", got, want)
	}
}

func TestXDRDecoderTruncated(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		read func(d *xdrDecoder)
	}{
		{"uint32 不足 4 字节", []byte{0, 0, 1}, func(d *xdrDecoder) { d.uint32() }},
		{"空缓冲区", nil, func(d *xdrDecoder) { d.int32() }},
		{"opaque 长度超出剩余数据", []byte{0, 0, 0, 8, 1, 2}, func(d *xdrDecoder) { d.opaque(16) }},
		{"opaque 缺补零字节", []byte{0, 0, 0, 1, 'a'}, func(d *xdrDecoder) { d.opaque(16) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newXDRDecoder(tc.buf)
			tc.read(d)
			if err := d.error(); err == nil {
				t.Fatal("期望解码出错，实际没有")
			} else if !errors.Is(err, errShortBuffer) {
				t.Logf("错误 = %v", err)
			}
		})
	}
}

// TestXDRDecoderOpaqueLimit 检查畸形长度不会导致巨额内存分配。
func TestXDRDecoderOpaqueLimit(t *testing.T) {
	// 声称有 4 GiB 数据，但报文只有 4 个字节。
	d := newXDRDecoder([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	got := d.opaque(1024)
	if got != nil {
		t.Errorf("期望返回 nil，实际 %d 字节", len(got))
	}
	if err := d.error(); err == nil {
		t.Fatal("期望超长报错")
	} else if !errors.Is(err, ErrProtocol) {
		t.Errorf("错误 = %v, 期望 ErrProtocol", err)
	}
}

func TestXDRPad(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 3, 2: 2, 3: 1, 4: 0, 5: 3, 8: 0} {
		if got := xdrPad(n); got != want {
			t.Errorf("xdrPad(%d) = %d, 期望 %d", n, got, want)
		}
	}
}
