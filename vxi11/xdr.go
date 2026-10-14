package vxi11

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 本文件实现 RFC 4506（XDR）中最小的一个子集，只覆盖 VXI-11 用得到的类型：
//
//	int / long / unsigned long  -> 4 字节大端有符号/无符号整数
//	bool                        -> 4 字节，0 或 1
//	char                        -> 4 字节（XDR 的 char 不是 1 字节）
//	unsigned short              -> 4 字节（XDR 没有 2 字节类型）
//	opaque<> / string<>         -> 4 字节长度 + 数据 + 补零到 4 字节对齐
//
// 所有变长数据的长度都在报文里，解码时必须有上限，否则一个畸形报文
// 就能让客户端申请几十 GB 内存。上限见 maxOpaqueLen。

// maxOpaqueLen 是解码变长数据（opaque<>/string<>）时允许的最大字节数。
const maxOpaqueLen = 64 << 20 // 64 MiB

// errShortBuffer 表示 XDR 数据不完整（报文被截断）。
var errShortBuffer = errors.New("vxi11: truncated XDR data")

// xdrPad 返回 n 字节的数据需要补齐多少个零字节才能 4 字节对齐。
func xdrPad(n int) int { return (4 - n%4) % 4 }

// xdrEncoder 把 Go 值按 XDR 规则追加到内部缓冲区。
type xdrEncoder struct {
	buf []byte
}

func (e *xdrEncoder) uint32(v uint32) {
	e.buf = binary.BigEndian.AppendUint32(e.buf, v)
}

func (e *xdrEncoder) int32(v int32) {
	e.uint32(uint32(v))
}

// uint16 按 XDR 规则编码为 4 字节（规范里的 unsigned short）。
func (e *xdrEncoder) uint16(v uint16) {
	e.uint32(uint32(v))
}

// char 按 XDR 规则编码为 4 字节（规范里的 char）。
func (e *xdrEncoder) char(c byte) {
	e.uint32(uint32(c))
}

func (e *xdrEncoder) boolean(v bool) {
	if v {
		e.uint32(1)
		return
	}
	e.uint32(0)
}

// opaque 编码 opaque<>：4 字节长度 + 数据 + 补零对齐。
func (e *xdrEncoder) opaque(b []byte) {
	e.uint32(uint32(len(b)))
	e.buf = append(e.buf, b...)
	if pad := xdrPad(len(b)); pad > 0 {
		e.buf = append(e.buf, make([]byte, pad)...)
	}
}

// string 编码 string<>，与 opaque<> 编码方式一致。
func (e *xdrEncoder) string(s string) {
	e.uint32(uint32(len(s)))
	e.buf = append(e.buf, s...)
	if pad := xdrPad(len(s)); pad > 0 {
		e.buf = append(e.buf, make([]byte, pad)...)
	}
}

// bytes 返回已编码的内容（不复制）。
func (e *xdrEncoder) bytes() []byte { return e.buf }

// xdrDecoder 从字节流按 XDR 规则解码。
//
// 它采用"记录第一个错误、后续读取返回零值"的策略：调用方可以连续读取多个字段，
// 最后统一检查 error()，从而避免每个字段都写一遍错误处理。
type xdrDecoder struct {
	buf []byte
	off int
	err error
}

func newXDRDecoder(b []byte) *xdrDecoder { return &xdrDecoder{buf: b} }

// error 返回解码过程中记录到的第一个错误。
func (d *xdrDecoder) error() error { return d.err }

// remaining 返回尚未消费的字节数。
func (d *xdrDecoder) remaining() int { return len(d.buf) - d.off }

func (d *xdrDecoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *xdrDecoder) need(n int) bool {
	if d.err != nil {
		return false
	}
	if d.remaining() < n {
		d.fail(fmt.Errorf("%w: need %d bytes, only %d left", errShortBuffer, n, d.remaining()))
		return false
	}
	return true
}

func (d *xdrDecoder) uint32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v
}

func (d *xdrDecoder) int32() int32 { return int32(d.uint32()) }

func (d *xdrDecoder) uint16() uint16 { return uint16(d.uint32()) }

func (d *xdrDecoder) char() byte { return byte(d.uint32()) }

func (d *xdrDecoder) boolean() bool { return d.uint32() != 0 }

// opaque 解码 opaque<>，并校验长度不超过 max。
// 返回的切片是内部缓冲区的视图，需要长期持有请自行复制。
func (d *xdrDecoder) opaque(max int) []byte {
	n := int(d.uint32())
	if d.err != nil {
		return nil
	}
	if n < 0 || n > max {
		d.fail(fmt.Errorf("%w: variable-length data is %d bytes, limit is %d", ErrProtocol, n, max))
		return nil
	}
	if !d.need(n) {
		return nil
	}
	b := d.buf[d.off : d.off+n]
	d.off += n
	pad := xdrPad(n)
	if pad > 0 {
		if !d.need(pad) {
			return nil
		}
		d.off += pad
	}
	return b
}

// string 解码 string<>，并校验长度不超过 max。
func (d *xdrDecoder) string(max int) string { return string(d.opaque(max)) }

// skipOpaque 跳过一个 opaque<>/string<>（用于跳过 RPC 认证字段）。
func (d *xdrDecoder) skipOpaque() {
	d.opaque(maxOpaqueLen)
}
