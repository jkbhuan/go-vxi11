package vxi11

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// 这些测试手工核对报文字节，确保 XDR 编码与规范 C.1 的字段顺序完全一致。
// 一旦有人改动了结构字段顺序，这里会立刻失败。

func TestCreateLinkParmsMarshal(t *testing.T) {
	p := createLinkParms{
		clientID:    1,
		lockDevice:  true,
		lockTimeout: 0,
		device:      "inst0",
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x01, // clientId = 1
		0x00, 0x00, 0x00, 0x01, // lockDevice = true
		0x00, 0x00, 0x00, 0x00, // lock_timeout = 0
		0x00, 0x00, 0x00, 0x05, // device 长度 = 5
		'i', 'n', 's', 't', '0', 0, 0, 0, // 数据 + 3 字节补零
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("create_link 参数编码 =\n% X\n期望\n% X", got, want)
	}
}

func TestDeviceWriteParmsMarshal(t *testing.T) {
	p := deviceWriteParms{
		lid:         2,
		ioTimeout:   5000,
		lockTimeout: 0,
		flags:       FlagEnd,
		data:        []byte("*IDN?\n"),
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x02, // lid
		0x00, 0x00, 0x13, 0x88, // io_timeout = 5000
		0x00, 0x00, 0x00, 0x00, // lock_timeout
		0x00, 0x00, 0x00, 0x08, // flags: bit3 = end
		0x00, 0x00, 0x00, 0x06, // data 长度 = 6
		'*', 'I', 'D', 'N', '?', '\n', 0, 0,
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("device_write 参数编码 =\n% X\n期望\n% X", got, want)
	}
}

func TestDeviceReadParmsMarshal(t *testing.T) {
	p := deviceReadParms{
		lid:         0,
		requestSize: 1024,
		ioTimeout:   5000,
		lockTimeout: 0,
		flags:       FlagTermChrSet,
		termChar:    '\n',
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x00, // lid
		0x00, 0x00, 0x04, 0x00, // requestSize = 1024
		0x00, 0x00, 0x13, 0x88, // io_timeout = 5000
		0x00, 0x00, 0x00, 0x00, // lock_timeout
		0x00, 0x00, 0x00, 0x80, // flags: bit7 = termchrset
		0x00, 0x00, 0x00, 0x0A, // termChar = '\n'
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("device_read 参数编码 =\n% X\n期望\n% X", got, want)
	}
}

// TestDeviceGenericParmsMarshal 核对字段顺序是
// lid, flags, lock_timeout, io_timeout（易与 Write/Read 混淆）。
func TestDeviceGenericParmsMarshal(t *testing.T) {
	p := deviceGenericParms{lid: 1, flags: FlagWaitLock, lockTimeout: 100, ioTimeout: 200}
	want := []byte{
		0x00, 0x00, 0x00, 0x01, // lid
		0x00, 0x00, 0x00, 0x01, // flags
		0x00, 0x00, 0x00, 0x64, // lock_timeout = 100
		0x00, 0x00, 0x00, 0xC8, // io_timeout = 200
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("Device_GenericParms 编码 =\n% X\n期望\n% X", got, want)
	}
}

func TestDeviceLockParmsMarshal(t *testing.T) {
	p := deviceLockParms{lid: 3, flags: FlagWaitLock, lockTimeout: 1000}
	want := []byte{
		0x00, 0x00, 0x00, 0x03,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x03, 0xE8,
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("device_lock 参数编码 =\n% X\n期望\n% X", got, want)
	}
}

func TestEnableSrqParmsMarshal(t *testing.T) {
	p := deviceEnableSrqParms{lid: 1, enable: false, handle: []byte{0xAA, 0xBB}}
	want := []byte{
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, // enable = false
		0x00, 0x00, 0x00, 0x02, // handle 长度
		0xAA, 0xBB, 0x00, 0x00, // 数据 + 补零
	}
	got := p.marshal()
	if !bytes.Equal(got, want) {
		t.Errorf("device_enable_srq 参数编码 =\n% X\n期望\n% X", got, want)
	}
}

func TestParseCreateLinkResp(t *testing.T) {
	e := &xdrEncoder{}
	e.int32(0)
	e.int32(-1)     // lid
	e.uint16(1024)  // abortPort（unsigned short 在 XDR 里也是 4 字节）
	e.uint32(65536) // maxRecvSize
	d := newXDRDecoder(e.bytes())
	got := parseCreateLinkResp(d)
	if err := d.error(); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.err != 0 || got.lid != -1 || got.abortPort != 1024 || got.maxRecvSize != 65536 {
		t.Errorf("解析结果 = %+v", got)
	}
	if d.remaining() != 0 {
		t.Errorf("剩余 %d 字节", d.remaining())
	}
}

func TestParseDeviceReadResp(t *testing.T) {
	e := &xdrEncoder{}
	e.int32(0)
	e.int32(int32(ReasonEnd | ReasonChr))
	e.opaque([]byte("abc"))
	d := newXDRDecoder(e.bytes())
	got := parseDeviceReadResp(d)
	if err := d.error(); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !got.reason.Has(ReasonEnd) || !got.reason.Has(ReasonChr) {
		t.Errorf("reason = %v", got.reason)
	}
	if got.reason.Has(ReasonReqCnt) {
		t.Error("不应含 REQCNT")
	}
	if string(got.data) != "abc" {
		t.Errorf("data = %q", got.data)
	}
}

// TestDeviceReadErrorMapping 检查响应里的错误码能映射为可判断的 Go error。
func TestDeviceReadErrorMapping(t *testing.T) {
	e := &xdrEncoder{}
	e.int32(int32(ErrInvalidLinkID))
	e.int32(0)
	e.opaque(nil)
	d := newXDRDecoder(e.bytes())
	got := parseDeviceReadResp(d)
	if err := d.error(); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	err := deviceErr("device_read", got.err)
	if !errors.Is(err, ErrInvalidLinkID) {
		t.Errorf("errors.Is(err, ErrInvalidLinkID) = false, err = %v", err)
	}
	var de *DeviceError
	if !errors.As(err, &de) {
		t.Fatalf("errors.As 失败: %v", err)
	}
	if de.Op != "device_read" || de.Code != ErrInvalidLinkID {
		t.Errorf("DeviceError = %+v", de)
	}
	// 错误码 0 应当映射为 nil。
	if err := deviceErr("device_read", 0); err != nil {
		t.Errorf("错误码 0 应返回 nil, 得到 %v", err)
	}
}

func TestFlagsString(t *testing.T) {
	cases := map[Flags]string{
		0:                                       "0",
		FlagEnd:                                 "end",
		FlagWaitLock:                            "waitlock",
		FlagTermChrSet:                          "termchrset",
		FlagWaitLock | FlagEnd:                  "waitlock|end",
		FlagWaitLock | FlagEnd | FlagTermChrSet: "waitlock|end|termchrset",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("Flags(%#x).String() = %q, 期望 %q", uint32(f), got, want)
		}
	}
}

func TestReadReasonString(t *testing.T) {
	cases := map[ReadReason]string{
		0:                        "0",
		ReasonReqCnt:             "REQCNT",
		ReasonChr:                "CHR",
		ReasonEnd:                "END",
		ReasonReqCnt | ReasonEnd: "REQCNT|END",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("ReadReason(%d).String() = %q, 期望 %q", int32(r), got, want)
		}
	}
}

func TestProcName(t *testing.T) {
	if got := procName(procDeviceWrite); got != "device_write" {
		t.Errorf("procName(11) = %q", got)
	}
	if got := procName(999); got != "proc(999)" {
		t.Errorf("procName(999) = %q", got)
	}
}

func TestDurationToMillis(t *testing.T) {
	cases := map[time.Duration]uint32{0: 0, -time.Second: 0, time.Millisecond: 1, 5 * time.Second: 5000}
	for d, want := range cases {
		if got := durationToMillis(d); got != want {
			t.Errorf("durationToMillis(%v) = %d, 期望 %d", d, got, want)
		}
	}
}
