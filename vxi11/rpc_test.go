package vxi11

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestRecordMarkingRoundTrip 验证记录标记的分片与重组。
func TestRecordMarkingRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 100) // 1000 字节
	var buf bytes.Buffer
	if err := writeRecord(&buf, payload, 256); err != nil {
		t.Fatalf("writeRecord 失败: %v", err)
	}

	// 1000 字节按 256 分片 -> 4 片（3*256 + 232），每片 4 字节头。
	if got, want := buf.Len(), 1000+4*4; got != want {
		t.Fatalf("编码长度 = %d, 期望 %d", got, want)
	}
	// 除最后一片外，分片头都不带"最后一片"标志。
	raw := buf.Bytes()
	for i := 0; i < 4; i++ {
		hdr := binary.BigEndian.Uint32(raw[i*(256+4):])
		if i < 3 && hdr&lastFragmentFlag != 0 {
			t.Errorf("第 %d 片不应带最后一片标志", i)
		}
		if i == 3 && hdr&lastFragmentFlag == 0 {
			t.Errorf("第 %d 片应带最后一片标志", i)
		}
	}

	got, err := readRecord(&buf, 1<<20)
	if err != nil {
		t.Fatalf("readRecord 失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("重组后数据不一致")
	}
}

func TestRecordMarkingEmptyAndSingle(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRecord(&buf, nil, 0); err != nil {
		t.Fatalf("writeRecord 失败: %v", err)
	}
	if got := binary.BigEndian.Uint32(buf.Bytes()); got != lastFragmentFlag {
		t.Errorf("空消息的分片头 = %#x, 期望 %#x", got, uint32(lastFragmentFlag))
	}
	got, err := readRecord(&buf, 1<<20)
	if err != nil {
		t.Fatalf("readRecord 失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空消息解出 %d 字节", len(got))
	}

	buf.Reset()
	if err := writeRecord(&buf, []byte("hello"), 1<<20); err != nil {
		t.Fatalf("writeRecord 失败: %v", err)
	}
	got, err = readRecord(&buf, 1<<20)
	if err != nil {
		t.Fatalf("readRecord 失败: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("解出 %q", got)
	}
}

// TestReadRecordLengthLimit 验证对端声称超大长度时不会分配巨量内存。
func TestReadRecordLengthLimit(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], lastFragmentFlag|0x7FFFFFFF) // 2 GiB
	buf.Write(hdr[:])
	if _, err := readRecord(&buf, 1<<20); !errors.Is(err, ErrProtocol) {
		t.Errorf("错误 = %v, 期望 ErrProtocol", err)
	}
}

// TestReadRecordEOF 验证对端半途关闭连接时的错误语义。
func TestReadRecordEOF(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], lastFragmentFlag|10)
	buf.Write(hdr[:])
	buf.WriteString("abc") // 只给了 3 字节
	_, err := readRecord(&buf, 1<<20)
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Errorf("错误 = %v, 期望 EOF 类错误", err)
	}
}

// TestRPCConnCallOverPipe 用一个内存管道（net.Pipe）手写应答，
// 验证调用消息编码、xid 校验与应答解析。
func TestRPCConnCallOverPipe(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	r := newRPCConn(client, deviceCoreProg, deviceCoreVers)

	// 服务端读到的调用头（在子 goroutine 里解析，通过 channel 交回主 goroutine 断言，
	// 避免在测试结束后调用 t.Errorf）。
	type callHeader struct {
		xid, mtype, rpcvers, prog, vers, proc, credFlavor, verfFlavor uint32
		parms                                                         createLinkParms
	}
	got := make(chan callHeader, 1)
	go func() {
		msg, err := readRecord(server, 1<<20)
		if err != nil {
			got <- callHeader{}
			return
		}
		d := newXDRDecoder(msg)
		var h callHeader
		h.xid = d.uint32()
		h.mtype = d.uint32()
		h.rpcvers = d.uint32()
		h.prog = d.uint32()
		h.vers = d.uint32()
		h.proc = d.uint32()
		h.credFlavor = d.uint32()
		d.skipOpaque()
		h.verfFlavor = d.uint32()
		d.skipOpaque()
		h.parms = createLinkParms{
			clientID:    d.int32(),
			lockDevice:  d.boolean(),
			lockTimeout: d.uint32(),
			device:      d.string(1024),
		}
		got <- h

		e := &xdrEncoder{}
		e.uint32(h.xid)
		e.uint32(msgTypeReply)
		e.uint32(replyStatAccepted)
		e.uint32(authNull)
		e.uint32(0)
		e.uint32(acceptStatSuccess)
		e.int32(0)     // Device_Error.error
		e.int32(7)     // lid
		e.uint16(5000) // abortPort
		e.uint32(8192) // maxRecvSize
		_ = writeRecord(server, e.bytes(), 1<<20)
	}()

	client.SetDeadline(time.Now().Add(2 * time.Second))

	parms := createLinkParms{clientID: 1234, lockDevice: true, lockTimeout: 50, device: "inst0"}
	d, err := r.call(procCreateLink, parms.marshal(), time.Second)
	if err != nil {
		t.Fatalf("call 失败: %v", err)
	}
	resp := parseCreateLinkResp(d)
	if err := d.error(); err != nil {
		t.Fatalf("应答解析失败: %v", err)
	}

	if resp.err != 0 || resp.lid != 7 || resp.abortPort != 5000 || resp.maxRecvSize != 8192 {
		t.Errorf("create_link 应答 = %+v, 期望 {0 7 5000 8192}", resp)
	}

	h := <-got
	switch {
	case h.mtype != msgTypeCall:
		t.Errorf("mtype = %d, 期望 MSG_CALL", h.mtype)
	case h.rpcvers != rpcVersion:
		t.Errorf("rpcvers = %d", h.rpcvers)
	case h.prog != deviceCoreProg:
		t.Errorf("prog = %#x, 期望 %#x", h.prog, uint32(deviceCoreProg))
	case h.vers != deviceCoreVers:
		t.Errorf("vers = %d", h.vers)
	case h.proc != procCreateLink:
		t.Errorf("proc = %d, 期望 %d", h.proc, procCreateLink)
	case h.credFlavor != authNull || h.verfFlavor != authNull:
		t.Errorf("认证方式 = %d/%d, 期望 AUTH_NULL", h.credFlavor, h.verfFlavor)
	case h.parms != parms:
		t.Errorf("create_link 参数 = %+v, 期望 %+v", h.parms, parms)
	}
}

func TestParseReplyErrors(t *testing.T) {
	mk := func(mod func(e *xdrEncoder)) []byte {
		e := &xdrEncoder{}
		e.uint32(42) // xid
		e.uint32(msgTypeReply)
		mod(e)
		return e.bytes()
	}

	t.Run("xid 不匹配", func(t *testing.T) {
		msg := mk(func(e *xdrEncoder) { e.uint32(replyStatAccepted) })
		_, err := parseReply(msg, 43)
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("错误 = %v, 期望 ErrProtocol", err)
		}
	})

	t.Run("程序不可用", func(t *testing.T) {
		msg := mk(func(e *xdrEncoder) {
			e.uint32(replyStatAccepted)
			e.uint32(authNull)
			e.uint32(0)
			e.uint32(acceptStatProgUnavail)
		})
		_, err := parseReply(msg, 42)
		if !errors.Is(err, ErrProgramUnavailable) {
			t.Errorf("错误 = %v, 期望 ErrProgramUnavailable", err)
		}
	})

	t.Run("过程不可用", func(t *testing.T) {
		msg := mk(func(e *xdrEncoder) {
			e.uint32(replyStatAccepted)
			e.uint32(authNull)
			e.uint32(0)
			e.uint32(acceptStatProcUnavail)
		})
		_, err := parseReply(msg, 42)
		if !errors.Is(err, ErrProcedureUnavailable) {
			t.Errorf("错误 = %v, 期望 ErrProcedureUnavailable", err)
		}
	})

	t.Run("认证被拒", func(t *testing.T) {
		msg := mk(func(e *xdrEncoder) {
			e.uint32(replyStatDenied)
			e.uint32(rejectStatAuthError)
		})
		_, err := parseReply(msg, 42)
		if !errors.Is(err, ErrAuthError) {
			t.Errorf("错误 = %v, 期望 ErrAuthError", err)
		}
	})

	t.Run("成功", func(t *testing.T) {
		msg := mk(func(e *xdrEncoder) {
			e.uint32(replyStatAccepted)
			e.uint32(authNull)
			e.uint32(0)
			e.uint32(acceptStatSuccess)
			e.int32(0)
			e.int32(99)
		})
		d, err := parseReply(msg, 42)
		if err != nil {
			t.Fatalf("错误 = %v", err)
		}
		if got := d.int32(); got != 0 {
			t.Errorf("第一个结果字段 = %d, 期望 0", got)
		}
		if got := d.int32(); got != 99 {
			t.Errorf("第二个结果字段 = %d, 期望 99", got)
		}
	})
}

func TestWrapNetError(t *testing.T) {
	timeoutErr := &net.DNSError{IsTimeout: true}
	if got := wrapNetError(timeoutErr); !errors.Is(got, ErrTimeout) {
		t.Errorf("超时错误 = %v, 期望 ErrTimeout", got)
	}
	if got := wrapNetError(io.ErrUnexpectedEOF); !errors.Is(got, ErrConnClosed) {
		t.Errorf("EOF 错误 = %v, 期望 ErrConnClosed", got)
	}
	if got := wrapNetError(errors.New("boom")); !errors.Is(got, ErrTransport) {
		t.Errorf("普通错误 = %v, 期望 ErrTransport", got)
	}
	if got := wrapNetError(ErrProtocol); !errors.Is(got, ErrProtocol) {
		t.Errorf("ErrProtocol 应原样返回, 得到 %v", got)
	}
}

// TestBrokenConnStaysBroken 验证传输层错误后连接会被标记为不可用。
func TestBrokenConnStaysBroken(t *testing.T) {
	client, server := net.Pipe()
	server.Close() // 对端立刻关闭

	r := newRPCConn(client, deviceCoreProg, deviceCoreVers)
	if _, err := r.call(procCreateLink, nil, time.Second); err == nil {
		t.Fatal("期望出错")
	} else if !errors.Is(err, ErrConnClosed) && !errors.Is(err, ErrTransport) {
		t.Errorf("错误 = %v", err)
	}
	_, err := r.call(procCreateLink, nil, time.Second)
	if !errors.Is(err, ErrConnBroken) {
		t.Errorf("第二次调用的错误 = %v, 期望 ErrConnBroken", err)
	}
	client.Close()
}

func TestDeadlineFor(t *testing.T) {
	if _, ok := deadlineFor(0); ok {
		t.Error("timeout=0 不应设置截止时间")
	}
	if _, ok := deadlineFor(-time.Second); ok {
		t.Error("负超时不应设置截止时间")
	}
	dl, ok := deadlineFor(time.Second)
	if !ok || time.Until(dl) <= 0 {
		t.Error("正超时应当得到未来的截止时间")
	}
}
