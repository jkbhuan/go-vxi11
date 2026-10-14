package vxi11

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 集成式单元测试：用 mockdevice_test.go 里的假仪器走完整的
// portmapper -> create_link -> write/read -> destroy_link 流程。

const testTimeout = 3 * time.Second

func dialMock(t *testing.T, m *mockDevice, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithPortmapperPort(m.port)}, opts...)
	c, err := Dial("127.0.0.1", "inst0", testTimeout, all...)
	if err != nil {
		t.Fatalf("Dial 失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDialAndQuery(t *testing.T) {
	m := startMock(t, mockConfig{
		responses: map[string]string{
			"*IDN?":       "MOCK,DS1000Z,E12345,1.00\n",
			":MEAS:FREQ?": "1.0000E+03\n",
		},
		maxRecvSize: 4096,
		abortPort:   true,
	})
	c := dialMock(t, m)

	if got, want := c.LinkID(), int32(1); got != want {
		t.Errorf("LinkID() = %d, 期望 %d", got, want)
	}
	if got, want := c.MaxRecvSize(), uint32(4096); got != want {
		t.Errorf("MaxRecvSize() = %d, 期望 %d", got, want)
	}
	if got, want := c.AbortPort(), uint16(m.port); got != want {
		t.Errorf("AbortPort() = %d, 期望 %d", got, want)
	}

	idn, err := c.Query("*IDN?")
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if idn != "MOCK,DS1000Z,E12345,1.00" {
		t.Errorf("Query(*IDN?) = %q", idn)
	}

	freq, err := c.Query(":MEAS:FREQ?")
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if freq != "1.0000E+03" {
		t.Errorf("Query(:MEAS:FREQ?) = %q", freq)
	}

	writes, commands, _ := m.snapshot()
	if len(commands) != 2 || commands[0] != "*IDN?" || commands[1] != ":MEAS:FREQ?" {
		t.Errorf("设备收到的命令 = %v", commands)
	}
	// Query 内部应把命令补上 '\n' 并以 END 结束。
	if n := len(writes); n == 0 || !writes[n-1].end {
		t.Errorf("最后一次写入没有 END 标志: %+v", writes)
	}
	for _, w := range writes {
		if !bytes.HasSuffix(w.data, []byte{'\n'}) {
			t.Errorf("写入内容未以 \\n 结尾: %q", w.data)
		}
	}
}

// TestQueryLongResponse 覆盖验收标准里的"超长响应分片拼接"：
// 响应远大于单次 device_read 的长度，客户端必须循环读取并拼接。
func TestQueryLongResponse(t *testing.T) {
	payload := make([]byte, 0, 30000)
	for i := 0; i < 30000; i++ {
		// 刻意混入 '\n'（0x0a）与 0，验证二进制安全。
		payload = append(payload, byte(i%251))
	}
	m := startMock(t, mockConfig{
		maxRecvSize: 1024,
		responses: map[string]string{
			":WAV:DATA?": string(payload),
		},
	})
	// 把单次 device_read 压到 1024 字节，强制走分片读取。
	c := dialMock(t, m, WithReadSize(1024))

	got, err := c.QueryBytes(":WAV:DATA?", testTimeout)
	if err != nil {
		t.Fatalf("QueryBytes 失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("长响应不一致: 收到 %d 字节，期望 %d 字节", len(got), len(payload))
	}
	_, _, stats := m.snapshot()
	if stats["reads"] < 30 {
		t.Errorf("device_read 只被调用 %d 次，说明没有分片读取", stats["reads"])
	}
}

// TestQueryReasonZero 覆盖规范 OBSERVATION B.6.9：
// device_read 可能返回数据但 reason 全 0，客户端必须继续读而不是就此结束。
func TestQueryReasonZero(t *testing.T) {
	want := "1234567890"
	m := startMock(t, mockConfig{
		maxRecvSize: 1024,
		responses:   map[string]string{":TEST?": want},
		// 前 3 次 device_read 返回 reason=0
		zeroReasonReads: 3,
	})
	// 故意把单次读取长度压到 4 字节，这样前 3 次都只能拿到一部分数据；
	// 如果客户端在 reason==0 时就收工，只会拿到 "1234"。
	c := dialMock(t, m, WithReadSize(4))

	got, err := c.QueryBytes(":TEST?", testTimeout)
	if err != nil {
		t.Fatalf("QueryBytes 失败: %v", err)
	}
	if string(got) != want {
		t.Errorf("QueryBytes = %q, 期望 %q", got, want)
	}
}

// TestWriteFragmentation 覆盖验收标准里"超过 maxRecvSize 的写入要分片，
// 只有最后一片带 end 标志"。
func TestWriteFragmentation(t *testing.T) {
	m := startMock(t, mockConfig{maxRecvSize: 1024})
	c := dialMock(t, m)

	data := bytes.Repeat([]byte("A"), 5000)
	n, err := c.Write(data, testTimeout)
	if err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write 返回 %d, 期望 %d", n, len(data))
	}

	writes, _, _ := m.snapshot()
	if len(writes) != 5 {
		t.Fatalf("分片数 = %d, 期望 5", len(writes))
	}
	var reassembled []byte
	for i, w := range writes {
		reassembled = append(reassembled, w.data...)
		last := i == len(writes)-1
		if w.end != last {
			t.Errorf("第 %d 片的 end = %v, 期望 %v", i, w.end, last)
		}
		if !last && len(w.data) != 1024 {
			t.Errorf("第 %d 片长度 = %d, 期望 1024", i, len(w.data))
		}
		if last && len(w.data) != 5000-4*1024 {
			t.Errorf("最后一片长度 = %d, 期望 %d", len(w.data), 5000-4*1024)
		}
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("分片重组后与原始数据不一致")
	}
	// 假仪器（和真机一样）会拒绝超过 maxRecvSize 的写入，这里必须为 0。
	if _, _, stats := m.snapshot(); stats["tooBig"] != 0 {
		t.Errorf("有 %d 次 device_write 超过 maxRecvSize", stats["tooBig"])
	}
}

// TestWriteNoFragmentation 验证 WithWriteChunkSize(0) 会关掉分片：
// 整条命令用一次 device_write 发出（部分仪器要求如此）。
//
// 这里把 maxRecvSize 放大，因为真机会拒绝超过 maxRecvSize 的单次写入——
// 关闭分片只适用于本身接受大包写入的仪器。
func TestWriteNoFragmentation(t *testing.T) {
	m := startMock(t, mockConfig{maxRecvSize: 8192})
	c := dialMock(t, m, WithWriteChunkSize(0))

	data := bytes.Repeat([]byte("B"), 5000)
	if _, err := c.Write(data, testTimeout); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	writes, _, _ := m.snapshot()
	if len(writes) != 1 {
		t.Fatalf("分片数 = %d, 期望 1（不分片）", len(writes))
	}
	if !writes[0].end || len(writes[0].data) != 5000 {
		t.Errorf("写入 = %d 字节, end = %v", len(writes[0].data), writes[0].end)
	}
}

// TestWriteChunkSizeOption 验证自定义分片大小。
func TestWriteChunkSizeOption(t *testing.T) {
	m := startMock(t, mockConfig{maxRecvSize: 1024})
	c := dialMock(t, m, WithWriteChunkSize(100))
	defer func() {
		// 假仪器会像真机一样拒绝超长写入，这里不应出现这种情况。
		if _, _, stats := m.snapshot(); stats["tooBig"] != 0 {
			t.Errorf("有 %d 次 device_write 超过 maxRecvSize", stats["tooBig"])
		}
	}()

	data := bytes.Repeat([]byte("C"), 250)
	if _, err := c.Write(data, testTimeout); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	writes, _, _ := m.snapshot()
	if len(writes) != 3 {
		t.Fatalf("分片数 = %d, 期望 3", len(writes))
	}
	for i, w := range writes {
		if want := 100; len(w.data) != want && i < 2 {
			t.Errorf("第 %d 片长度 = %d, 期望 %d", i, len(w.data), want)
		}
		if w.end != (i == 2) {
			t.Errorf("第 %d 片的 end = %v", i, w.end)
		}
	}
}

// TestDefaultReadSize 确认默认不跟随 maxRecvSize：maxRecvSize 只约束
// device_write，用来限制 device_read 会让大响应慢很多。
func TestDefaultReadSize(t *testing.T) {
	m := startMock(t, mockConfig{maxRecvSize: 1024})
	c := dialMock(t, m)
	if got, want := c.readRequestSize(), uint32(defaultReadSize); got != want {
		t.Errorf("默认读取长度 = %d, 期望 %d", got, want)
	}

	// 显式指定时以指定值为准。
	c2 := dialMock(t, m, WithReadSize(2048))
	if got, want := c2.readRequestSize(), uint32(2048); got != want {
		t.Errorf("WithReadSize(2048) 后 = %d", got)
	}
}

func TestWriteStringAddsNewlineOnce(t *testing.T) {
	m := startMock(t, mockConfig{})
	c := dialMock(t, m)

	if err := c.WriteString(":RUN"); err != nil {
		t.Fatalf("WriteString 失败: %v", err)
	}
	if err := c.WriteString(":STOP\n"); err != nil {
		t.Fatalf("WriteString 失败: %v", err)
	}
	writes, _, _ := m.snapshot()
	if len(writes) != 2 {
		t.Fatalf("写入次数 = %d, 期望 2", len(writes))
	}
	if string(writes[0].data) != ":RUN\n" {
		t.Errorf("第一次写入 = %q", writes[0].data)
	}
	if string(writes[1].data) != ":STOP\n" {
		t.Errorf("第二次写入 = %q（不应重复追加换行）", writes[1].data)
	}
}

func TestDialDeviceNotAccessible(t *testing.T) {
	m := startMock(t, mockConfig{device: "inst0"})
	_, err := Dial("127.0.0.1", "wrong", testTimeout, WithPortmapperPort(m.port))
	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if !errors.Is(err, ErrDeviceNotAccessible) {
		t.Errorf("错误 = %v, 期望 errors.Is(..., ErrDeviceNotAccessible)", err)
	}
	de, ok := AsDeviceError(err)
	if !ok {
		t.Fatalf("AsDeviceError 失败: %v", err)
	}
	if de.Op != "create_link" || de.Code != ErrDeviceNotAccessible {
		t.Errorf("DeviceError = %+v", de)
	}
}

func TestPortmapperNotRegistered(t *testing.T) {
	m := startMock(t, mockConfig{noPortmap: true})
	_, err := Dial("127.0.0.1", "inst0", testTimeout, WithPortmapperPort(m.port))
	if !errors.Is(err, ErrPortmapper) {
		t.Errorf("错误 = %v, 期望 ErrPortmapper", err)
	}
}

func TestWithCorePortSkipsPortmapper(t *testing.T) {
	m := startMock(t, mockConfig{})
	c, err := Dial("127.0.0.1", "inst0", testTimeout, WithCorePort(m.port))
	if err != nil {
		t.Fatalf("Dial(WithCorePort) 失败: %v", err)
	}
	defer c.Close()
	if _, err := c.Query("*IDN?"); err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
}

func TestLockUnlock(t *testing.T) {
	m := startMock(t, mockConfig{})
	c := dialMock(t, m)

	if err := c.Lock(0); err != nil {
		t.Fatalf("Lock 失败: %v", err)
	}
	if err := c.Unlock(); err != nil {
		t.Fatalf("Unlock 失败: %v", err)
	}
	if err := c.Unlock(); !errors.Is(err, ErrNoLockHeld) {
		t.Errorf("重复 Unlock 的错误 = %v, 期望 ErrNoLockHeld", err)
	}
}

func TestLockConflict(t *testing.T) {
	m := startMock(t, mockConfig{lockBusy: true})
	c := dialMock(t, m)

	err := c.Lock(0)
	if !errors.Is(err, ErrDeviceLocked) {
		t.Fatalf("Lock 的错误 = %v, 期望 ErrDeviceLocked", err)
	}
	if !IsDeviceLocked(err) {
		t.Error("IsDeviceLocked 应返回 true")
	}

	// 带 waitlock 的加锁应当等待后成功。
	start := time.Now()
	if err := c.Lock(50 * time.Millisecond); err != nil {
		t.Fatalf("Lock(waitlock) 失败: %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("Lock(waitlock) 没有等待")
	}
	if err := c.Unlock(); err != nil {
		t.Fatalf("Unlock 失败: %v", err)
	}
}

func TestLockOnCreate(t *testing.T) {
	m := startMock(t, mockConfig{lockBusy: true})
	// lockDevice=true 且不等待 -> create_link 直接失败（错误码 11）。
	_, err := Dial("127.0.0.1", "inst0", testTimeout,
		WithPortmapperPort(m.port), WithLockOnCreate(true, 0))
	if !errors.Is(err, ErrDeviceLocked) {
		t.Fatalf("错误 = %v, 期望 ErrDeviceLocked", err)
	}

	// 等待 50ms 后应当成功。
	c, err := Dial("127.0.0.1", "inst0", testTimeout,
		WithPortmapperPort(m.port), WithLockOnCreate(true, 50*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial(lock OnCreate, 等待) 失败: %v", err)
	}
	defer c.Close()
	if err := c.Unlock(); err != nil {
		t.Fatalf("Unlock 失败: %v", err)
	}
}

func TestClearTriggerRemoteLocalSTB(t *testing.T) {
	m := startMock(t, mockConfig{stb: 0x42})
	c := dialMock(t, m)

	if _, err := c.Write([]byte(":RUN\n"), 0); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear 失败: %v", err)
	}
	if err := c.Trigger(); err != nil {
		t.Fatalf("Trigger 失败: %v", err)
	}
	if err := c.Remote(); err != nil {
		t.Fatalf("Remote 失败: %v", err)
	}
	if err := c.Local(); err != nil {
		t.Fatalf("Local 失败: %v", err)
	}
	stb, err := c.ReadSTB()
	if err != nil {
		t.Fatalf("ReadSTB 失败: %v", err)
	}
	if stb != 0x42 {
		t.Errorf("ReadSTB = %#x, 期望 0x42", stb)
	}
	if err := c.EnableSRQ(true, []byte("handle")); err != nil {
		t.Fatalf("EnableSRQ 失败: %v", err)
	}
	if err := c.EnableSRQ(true, make([]byte, 41)); err == nil {
		t.Error("EnableSRQ 应拒绝超过 40 字节的 handle")
	}

	_, _, stats := m.snapshot()
	if stats["clears"] != 1 {
		t.Errorf("device_clear 次数 = %d, 期望 1", stats["clears"])
	}
	if stats["triggers"] != 1 {
		t.Errorf("device_trigger 次数 = %d, 期望 1", stats["triggers"])
	}
}

func TestAbort(t *testing.T) {
	m := startMock(t, mockConfig{abortPort: true})
	c := dialMock(t, m)
	if err := c.Abort(); err != nil {
		t.Fatalf("Abort 失败: %v", err)
	}
	_, _, stats := m.snapshot()
	if stats["aborts"] != 1 {
		t.Errorf("device_abort 次数 = %d, 期望 1", stats["aborts"])
	}

	// 设备没有通告 abort 端口时应当给出明确错误。
	m2 := startMock(t, mockConfig{abortPort: false})
	c2 := dialMock(t, m2)
	if err := c2.Abort(); !errors.Is(err, ErrNoAbortChannel) {
		t.Errorf("Abort 的错误 = %v, 期望 ErrNoAbortChannel", err)
	}
}

func TestDocmd(t *testing.T) {
	// 设备支持 device_docmd：应返回 data_out。
	m := startMock(t, mockConfig{docmdSupported: true, docmdResp: []byte{1, 2, 3, 4}})
	c := dialMock(t, m)
	out, err := c.Docmd(7, false, 1, []byte{9, 9}, 0)
	if err != nil {
		t.Fatalf("Docmd 失败: %v", err)
	}
	if !bytes.Equal(out, []byte{1, 2, 3, 4}) {
		t.Errorf("Docmd data_out = %v, 期望 [1 2 3 4]", out)
	}

	// 设备不支持该 cmd：应返回错误码 8。
	m2 := startMock(t, mockConfig{docmdSupported: false})
	c2 := dialMock(t, m2)
	_, err = c2.Docmd(7, false, 1, nil, 0)
	if !errors.Is(err, ErrOperationNotSupported) {
		t.Errorf("Docmd 的错误 = %v, 期望 ErrOperationNotSupported", err)
	}
	var de *DeviceError
	if !errors.As(err, &de) || de.Op != "device_docmd" {
		t.Errorf("Docmd 错误应携带 op=device_docmd，实际 %v", err)
	}
}

func TestIntrChannel(t *testing.T) {
	m := startMock(t, mockConfig{intrChan: true})
	c := dialMock(t, m)

	got := make(chan []byte, 1)
	c.OnSRQ(func(handle []byte) { got <- handle })

	if err := c.CreateIntrChan(); err != nil {
		t.Fatalf("CreateIntrChan 失败: %v", err)
	}
	// 幂等：重复建立直接返回 nil。
	if err := c.CreateIntrChan(); err != nil {
		t.Fatalf("重复 CreateIntrChan 失败: %v", err)
	}
	_, _, stats := m.snapshot()
	if stats["intrCreates"] != 1 {
		t.Errorf("create_intr_chan 次数 = %d, 期望 1（重复调用应短路）", stats["intrCreates"])
	}

	// 假仪器在 device_enable_srq(enable=true) 时会回连发 device_intr_srq。
	handle := []byte("my-link-handle")
	if err := c.EnableSRQ(true, handle); err != nil {
		t.Fatalf("EnableSRQ 失败: %v", err)
	}
	select {
	case h := <-got:
		if !bytes.Equal(h, handle) {
			t.Errorf("SRQ handle = %q, 期望 %q", h, handle)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3 秒内没有收到 device_intr_srq")
	}

	if err := c.DestroyIntrChan(); err != nil {
		t.Fatalf("DestroyIntrChan 失败: %v", err)
	}
	// 幂等：重复拆除返回 nil。
	if err := c.DestroyIntrChan(); err != nil {
		t.Fatalf("重复 DestroyIntrChan 失败: %v", err)
	}
}

// TestIntrChannelNotSupported 验证设备不支持 interrupt channel 时的错误处理。
func TestIntrChannelNotSupported(t *testing.T) {
	m := startMock(t, mockConfig{intrChan: false})
	c := dialMock(t, m)
	if err := c.CreateIntrChan(); !errors.Is(err, ErrOperationNotSupported) {
		t.Errorf("CreateIntrChan 的错误 = %v, 期望 ErrOperationNotSupported", err)
	}
	// 建立失败后 DestroyIntrChan 应当幂等返回 nil（本地本来就没建立）。
	if err := c.DestroyIntrChan(); err != nil {
		t.Errorf("DestroyIntrChan 失败: %v", err)
	}
}

// TestCloseDestroysIntrChannel 验证 Close 会顺带拆除 interrupt channel。
func TestCloseDestroysIntrChannel(t *testing.T) {
	m := startMock(t, mockConfig{intrChan: true})
	c := dialMock(t, m)
	if err := c.CreateIntrChan(); err != nil {
		t.Fatalf("CreateIntrChan 失败: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	m.mu.Lock()
	up := m.intrChanUp
	m.mu.Unlock()
	if up {
		t.Error("Close 之后设备侧 interrupt channel 仍处于建立状态")
	}
}

func TestReadTimeoutAndBrokenConnection(t *testing.T) {
	m := startMock(t, mockConfig{
		readDelay: 500 * time.Millisecond,
	})
	c := dialMock(t, m)

	_, err := c.QueryWithTimeout("*IDN?", 20*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("错误 = %v, 期望 ErrTimeout", err)
	}
	if !IsTimeout(err) {
		t.Error("IsTimeout 应返回 true")
	}

	// 字节流已经错位，后续调用应当立刻失败，而不是读到上一次的应答。
	_, err = c.QueryWithTimeout("*IDN?", 20*time.Millisecond)
	if !errors.Is(err, ErrConnBroken) {
		t.Errorf("错误 = %v, 期望 ErrConnBroken", err)
	}
}

func TestDeviceErrorIsNotFatal(t *testing.T) {
	m := startMock(t, mockConfig{failRead: true})
	c := dialMock(t, m)

	_, err := c.Query("*IDN?")
	if !errors.Is(err, ErrIOError) {
		t.Fatalf("错误 = %v, 期望 ErrIOError", err)
	}
	// 设备错误不破坏字节流，连接仍然可用。
	_, err = c.Query("*IDN?")
	if !errors.Is(err, ErrIOError) {
		t.Errorf("第二次错误 = %v, 期望仍是 ErrIOError（说明连接未被标记为损坏）", err)
	}
}

func TestClose(t *testing.T) {
	m := startMock(t, mockConfig{abortPort: true})
	c := dialMock(t, m)

	if err := c.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	_, _, stats := m.snapshot()
	if stats["destroys"] != 1 {
		t.Errorf("destroy_link 次数 = %d, 期望 1", stats["destroys"])
	}
	// 幂等
	if err := c.Close(); err != nil {
		t.Errorf("重复 Close 返回 %v, 期望 nil", err)
	}
	if _, err := c.Query("*IDN?"); !errors.Is(err, ErrClosed) {
		t.Errorf("Close 后的 Query 错误 = %v, 期望 ErrClosed", err)
	}
	if err := c.WriteString(":RUN"); !errors.Is(err, ErrClosed) {
		t.Errorf("Close 后的 WriteString 错误 = %v, 期望 ErrClosed", err)
	}
	if err := c.Lock(0); !errors.Is(err, ErrClosed) {
		t.Errorf("Close 后的 Lock 错误 = %v, 期望 ErrClosed", err)
	}
	_, _, stats = m.snapshot()
	if stats["destroys"] != 1 {
		t.Errorf("destroy_link 次数 = %d, 期望仍为 1", stats["destroys"])
	}
}

func TestConcurrentQueries(t *testing.T) {
	m := startMock(t, mockConfig{
		maxRecvSize: 1024,
		responses:   map[string]string{"*IDN?": "MOCK,DS1000Z,E12345,1.00\n"},
	})
	c := dialMock(t, m)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				s, err := c.Query("*IDN?")
				if err != nil {
					errs[i] = err
					return
				}
				results[i] = s
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d 出错: %v", i, errs[i])
		}
		if results[i] != "MOCK,DS1000Z,E12345,1.00" {
			t.Errorf("goroutine %d 得到 %q", i, results[i])
		}
	}
}

func TestReadSingleAndReadAll(t *testing.T) {
	m := startMock(t, mockConfig{
		maxRecvSize: 1024,
		responses:   map[string]string{":BIG?": strings.Repeat("x", 3000)},
	})
	c := dialMock(t, m, WithReadSize(1024))

	if err := c.WriteString(":BIG?"); err != nil {
		t.Fatalf("WriteString 失败: %v", err)
	}
	// 单次 Read 只取一个分片。
	one, reason, err := c.ReadWithReason(0, testTimeout)
	if err != nil {
		t.Fatalf("ReadWithReason 失败: %v", err)
	}
	if len(one) != 1024 {
		t.Errorf("单次 Read 长度 = %d, 期望 1024", len(one))
	}
	if !reason.Has(ReasonReqCnt) {
		t.Errorf("reason = %v, 期望含 REQCNT", reason)
	}

	// ReadAll 把剩下的读完。
	rest, err := c.ReadAll(0, testTimeout)
	if err != nil {
		t.Fatalf("ReadAll 失败: %v", err)
	}
	if len(rest) != 3000-1024 {
		t.Errorf("ReadAll 长度 = %d, 期望 %d", len(rest), 3000-1024)
	}
	if all := string(one) + string(rest); all != strings.Repeat("x", 3000) {
		t.Error("拼接结果不正确")
	}
}

func TestReadAllRespectsMaxResponse(t *testing.T) {
	m := startMock(t, mockConfig{
		maxRecvSize: 1024,
		responses:   map[string]string{":BIG?": strings.Repeat("y", 5000)},
	})
	c, err := Dial("127.0.0.1", "inst0", testTimeout,
		WithPortmapperPort(m.port), WithMaxResponseBytes(2048))
	if err != nil {
		t.Fatalf("Dial 失败: %v", err)
	}
	defer c.Close()

	data, err := c.QueryBytes(":BIG?\n", testTimeout)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("错误 = %v, 期望 ErrResponseTooLarge", err)
	}
	if len(data) != 2048 {
		t.Errorf("返回数据长度 = %d, 期望 2048", len(data))
	}
}

func TestErrorCodeString(t *testing.T) {
	if got := ErrDeviceLocked.Error(); got != "device error 11 (device locked by another link)" {
		t.Errorf("Error() = %q", got)
	}
	if got := ErrorCode(1234).String(); got != "error code 1234" {
		t.Errorf("未知错误码的 String() = %q", got)
	}
	if err := ErrNoError.AsError(); err != nil {
		t.Errorf("ErrNoError.AsError() = %v, 期望 nil", err)
	}
}
