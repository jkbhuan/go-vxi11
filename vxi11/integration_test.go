//go:build integration

// 针对真实仪器的集成测试。默认不编译、不运行，需要显式指定 build tag：
//
//	go test -tags integration -v ./vxi11/ -host 5.5.5.1
//
// 或用环境变量：
//
//	VXI11_HOST=5.5.5.1 VXI11_DEVICE=inst0 go test -tags integration -v ./vxi11/
//
// 注意：这些测试会向仪器发送 SCPI 命令（*IDN? 等），请确认仪器处于可安全查询的状态。
package vxi11

import (
	"errors"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	flagHost   = flag.String("host", "", "被测仪器 IP，例如 5.5.5.1")
	flagDevice = flag.String("device", "", "VXI-11 设备名，默认 inst0")
	flagTO     = flag.Duration("io-timeout", 10*time.Second, "I/O 超时")
)

func envOr(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}

// dialDevice 按命令行/环境变量连接真实仪器；没有配置时跳过测试。
func dialDevice(t *testing.T) *Client {
	t.Helper()
	host := envOr(*flagHost, "VXI11_HOST")
	if host == "" {
		t.Skip("未指定仪器地址：请用 -host 参数或 VXI11_HOST 环境变量，例如 -host 5.5.5.1")
	}
	device := envOr(*flagDevice, "VXI11_DEVICE")
	c, err := Dial(host, device, *flagTO)
	if err != nil {
		t.Fatalf("Dial(%s) 失败: %v", host, err)
	}
	t.Cleanup(func() { c.Close() })
	t.Logf("已连接: %s", c)
	return c
}

// TestIntegrationPortmapAndCreateLink 对应调试步骤 1、2：
// portmapper 端口发现 + create_link。
func TestIntegrationPortmapAndCreateLink(t *testing.T) {
	c := dialDevice(t)
	if c.LinkID() == 0 && c.MaxRecvSize() == 0 {
		t.Error("create_link 返回的 lid 与 maxRecvSize 都是 0，可疑")
	}
	if c.Port() <= 0 || c.Port() > 65535 {
		t.Errorf("DEVICE_CORE 端口 = %d", c.Port())
	}
	t.Logf("core channel 端口 = %d, lid = %d, maxRecvSize = %d, abortPort = %d",
		c.Port(), c.LinkID(), c.MaxRecvSize(), c.AbortPort())
}

// TestIntegrationQueryIDN 对应调试步骤 3 与验收标准：
// Query("*IDN?") 能拿到仪器识别字符串。
func TestIntegrationQueryIDN(t *testing.T) {
	c := dialDevice(t)
	idn, err := c.Query("*IDN?")
	if err != nil {
		t.Fatalf("Query(*IDN?) 失败: %v", err)
	}
	if strings.TrimSpace(idn) == "" {
		t.Fatal("*IDN? 返回空字符串")
	}
	t.Logf("*IDN? -> %q", idn)
	// 常见格式：厂商,型号,序列号,固件版本
	if n := len(strings.Split(idn, ",")); n < 2 {
		t.Logf("提示：*IDN? 只返回了 %d 个字段，可能仪器实现不规范", n)
	}
}

// TestIntegrationReadSTB 验证 device_readstb。
// 该过程是可选的：实测 RIGOL DS1000Z 系列返回错误码 8（不支持），此时跳过。
func TestIntegrationReadSTB(t *testing.T) {
	c := dialDevice(t)
	stb, err := c.ReadSTB()
	if errors.Is(err, ErrOperationNotSupported) {
		t.Skip("该仪器不支持 device_readstb（错误码 8），请改用 SCPI 的 *STB?")
	}
	if err != nil {
		t.Fatalf("ReadSTB 失败: %v", err)
	}
	t.Logf("状态字节 = %#02x", stb)
}

// TestIntegrationLockUnlock 验证 device_lock / device_unlock。
func TestIntegrationLockUnlock(t *testing.T) {
	c := dialDevice(t)
	if err := c.Lock(0); err != nil {
		t.Fatalf("Lock 失败: %v", err)
	}
	if err := c.Unlock(); err != nil {
		t.Fatalf("Unlock 失败: %v", err)
	}
}

// TestIntegrationClear 验证 device_clear。
func TestIntegrationClear(t *testing.T) {
	c := dialDevice(t)
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear 失败: %v", err)
	}
}

// TestIntegrationLongResponse 对应验收标准里"超长响应分片拼接"：
// 读一段远大于 maxRecvSize 的数据，验证多次 device_read 的拼接不会截断。
//
// 默认命令 :DISP:DATA?（RIGOL 等屏幕截图，IEEE 488.2 定长块）；
// 设备不认识该命令（读超时）时自动退到泰克路线：
// SAVe:IMAGe 保存截图到仪器本地，再 FILESystem:READFile 读回。
// 也可用 VXI11_LONG_CMD 显式指定长响应命令。
func TestIntegrationLongResponse(t *testing.T) {
	c := dialDevice(t)
	if testing.Short() {
		t.Skip("short 模式跳过")
	}

	cmd := envOr("", "VXI11_LONG_CMD")
	if cmd == "" {
		cmd = ":DISP:DATA?"
	}

	start := time.Now()
	data, err := c.QueryBytes(cmd+"\n", 30*time.Second)
	if err != nil && envOr("", "VXI11_LONG_CMD") == "" && IsTimeout(err) {
		t.Logf("%s 读超时（该仪器可能不支持），尝试泰克截图路线", cmd)
		if werr := c.WriteString("SAVe:IMAGe \"C:/Temp/vxi11_test.png\""); werr != nil {
			t.Fatalf("SAVe:IMAGe 失败: %v", werr)
		}
		time.Sleep(3 * time.Second) // 等仪器把截图写完
		cmd = "FILESystem:READFile 'C:/Temp/vxi11_test.png'"
		start = time.Now()
		data, err = c.QueryBytes(cmd+"\n", 30*time.Second)
	}
	if err != nil {
		t.Fatalf("QueryBytes(%q) 失败: %v", cmd, err)
	}
	t.Logf("%s 返回 %d 字节（maxRecvSize=%d，读取耗时 %v）",
		cmd, len(data), c.MaxRecvSize(), time.Since(start).Round(time.Millisecond))

	if len(data) == 0 {
		t.Fatal("返回数据为空")
	}
	if len(data) <= int(c.MaxRecvSize()) {
		t.Logf("提示：本次响应未超过 maxRecvSize，没有真正走到分片路径")
		return
	}

	// 解析 IEEE 488.2 定长块头，核对声明长度与实际长度一致。
	if data[0] == '#' {
		n := int(data[1] - '0')
		declared, convErr := strconv.Atoi(string(data[2 : 2+n]))
		if convErr != nil {
			t.Fatalf("定长块长度字段非法: %v", convErr)
		}
		body := data[2+n:]
		t.Logf("定长块声明 %d 字节，实际收到 %d 字节", declared, len(body))
		if declared > len(body) {
			t.Errorf("数据被截断：声明 %d 字节，只有 %d 字节", declared, len(body))
		}
	} else {
		t.Logf("前 16 字节: % x", data[:min(16, len(data))])
	}
}

// queryErrorQueue 查询仪器错误队列：优先 :SYST:ERR?（RIGOL 等），
// 设备端超时（该命令不存在）时退到泰克的 ALLEv?。可用 VXI11_ERR_CMD 覆盖。
func queryErrorQueue(t *testing.T, c *Client) (string, error) {
	t.Helper()
	if cmd := envOr("", "VXI11_ERR_CMD"); cmd != "" {
		return c.Query(cmd)
	}
	line, err := c.Query(":SYST:ERR?")
	if err == nil {
		return line, nil
	}
	if !IsTimeout(err) {
		return "", err
	}
	t.Logf(":SYST:ERR? 超时（该仪器可能不支持），改用 ALLEv?")
	return c.Query("ALLEv?")
}

// TestIntegrationWriteFragmentation 对应验收标准里"超过 maxRecvSize 的写入要分片"。
//
// 构造一条长度超过 maxRecvSize 的合法 SCPI 命令链（重复的 *CLS;，幂等且无副作用），
// 写入后检查仪器错误队列，确认分片写入没有把命令写坏。
func TestIntegrationWriteFragmentation(t *testing.T) {
	c := dialDevice(t)

	// 载荷是重复的 *CLS（幂等、无副作用），长度超过 maxRecvSize 以触发分片。
	// 注意：命令分隔符 ';' 后面必须还有命令，不能直接跟消息终止符，
	// 否则部分仪器（实测 RIGOL DS1000Z）会报 -101 Invalid character。
	unit := "*CLS;"
	reps := 2*int(c.MaxRecvSize())/len(unit) + 1
	payload := strings.Repeat(unit, reps) + "*CLS\n"
	if len(payload) <= int(c.MaxRecvSize()) {
		t.Fatalf("测试载荷 %d 字节，未超过 maxRecvSize=%d", len(payload), c.MaxRecvSize())
	}

	n, err := c.Write([]byte(payload), *flagTO)
	if err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if n != len(payload) {
		t.Errorf("写入 %d 字节，期望 %d", n, len(payload))
	}
	t.Logf("一次 Write 发送 %d 字节，按 maxRecvSize=%d 分成约 %d 个 device_write",
		n, c.MaxRecvSize(), (n+int(c.MaxRecvSize())-1)/int(c.MaxRecvSize()))

	line, err := queryErrorQueue(t, c)
	if err != nil {
		t.Fatalf("查询仪器错误队列失败: %v", err)
	}
	t.Logf("仪器错误队列: %q", line)
	if !strings.Contains(line, "No error") && !strings.Contains(line, "No events") &&
		!strings.HasPrefix(strings.TrimSpace(line), "0,") {
		t.Errorf("分片写入后仪器报错：%q", line)
	}
}

// TestIntegrationAbortChannel 验证 abort channel（如果仪器提供）。
//
// 已知真机差异：
//   - RIGOL DS1000Z 通告 abortPort，但 device_abort 返回错误码 8（不支持）；
//   - Tektronix MSO58 通告 abortPort=7001，但 device_abort 返回错误码 4
//     （invalid link identifier），且在有在途 device_read 时根本不应答。
//     两者都属于设备侧实现缺陷，跳过而不是判失败。
func TestIntegrationAbortChannel(t *testing.T) {
	c := dialDevice(t)
	if c.AbortPort() == 0 {
		t.Skip("仪器未通告 abortPort")
	}
	err := c.Abort()
	switch {
	case errors.Is(err, ErrOperationNotSupported):
		t.Skipf("仪器通告了 abortPort=%d 但不支持 device_abort（错误码 8）", c.AbortPort())
	case errors.Is(err, ErrInvalidLinkID):
		t.Skipf("仪器通告了 abortPort=%d 但 device_abort 报 invalid link identifier（错误码 4），设备侧实现缺陷", c.AbortPort())
	case IsTimeout(err):
		t.Skipf("仪器通告了 abortPort=%d 但 device_abort 无应答，设备侧实现缺陷", c.AbortPort())
	case err != nil:
		t.Fatalf("Abort 失败: %v", err)
	}
	t.Logf("abort channel（端口 %d）可用", c.AbortPort())
}

// TestIntegrationDocmd 验证 device_docmd。
// cmd 的取值由厂商定义，绝大多数仪器不支持任何 cmd（返回错误码 8），
// 此时跳过；重要的是编解码与错误路径能走通。
func TestIntegrationDocmd(t *testing.T) {
	c := dialDevice(t)
	_, err := c.Docmd(0, false, 1, nil, *flagTO)
	if errors.Is(err, ErrOperationNotSupported) {
		t.Skip("仪器不支持 device_docmd 的 cmd=0（错误码 8，预期行为）")
	}
	if err != nil {
		t.Fatalf("Docmd 失败: %v", err)
	}
	t.Log("仪器支持 device_docmd cmd=0")
}

// TestIntegrationIntrSRQ 验证 interrupt channel 全链路：
// create_intr_chan -> device_enable_srq -> 触发 SRQ -> 收到 device_intr_srq。
//
// 触发方式用 IEEE 488.2 标准序列：*ESE 1（使能"操作完成"事件）、
// *SRE 32（ESB 位触发 SRQ）、*OPC（命令执行完置位"操作完成"）。
func TestIntegrationIntrSRQ(t *testing.T) {
	c := dialDevice(t)

	got := make(chan []byte, 1)
	c.OnSRQ(func(handle []byte) { got <- handle })

	if err := c.CreateIntrChan(); err != nil {
		if errors.Is(err, ErrOperationNotSupported) || errors.Is(err, ErrChannelNotEstablished) {
			t.Skipf("仪器不支持建立 interrupt channel: %v", err)
		}
		t.Fatalf("CreateIntrChan 失败: %v", err)
	}
	defer c.DestroyIntrChan()

	handle := []byte("go-vxi-srq-test")
	if err := c.EnableSRQ(true, handle); err != nil {
		if errors.Is(err, ErrOperationNotSupported) {
			t.Skip("仪器不支持 device_enable_srq（错误码 8）")
		}
		t.Fatalf("EnableSRQ 失败: %v", err)
	}
	defer c.EnableSRQ(false, nil)

	// 触发一次 SRQ。写入用带 END 的整条命令，避免与回调读抢通道。
	if err := c.WriteString("*ESE 1; *SRE 32; *OPC"); err != nil {
		t.Fatalf("写入 SRQ 触发序列失败: %v", err)
	}

	select {
	case h := <-got:
		t.Logf("收到 device_intr_srq，handle = %q", h)
		if !strings.EqualFold(string(h), string(handle)) {
			t.Errorf("SRQ handle = %q，期望 %q", h, handle)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("10 秒内没有收到 device_intr_srq")
	}

	// 收尾：关掉事件使能，避免残留 SRQ 影响后续测试。
	if err := c.WriteString("*SRE 0; *ESE 0"); err != nil {
		t.Logf("收尾写入失败（不影响判定）: %v", err)
	}
}
