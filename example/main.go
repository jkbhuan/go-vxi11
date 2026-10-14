// Command vxi11demo 是 go-vxi 包的演示程序，覆盖任务说明里"调试步骤"的完整闭环：
//
//	portmapper 端口发现 -> create_link -> device_write("*IDN?") -> device_read(分片) ->
//	device_lock/device_unlock -> device_clear -> destroy_link
//
// 用法（默认连接任务说明里的调试设备 5.5.5.1）：
//
//	go run ./example                         # 只做 *IDN? 查询
//	go run ./example -host 192.168.1.10 -q ':SYST:ERR?'
//	go run ./example -lock -bigwrite 3000    # 额外演示加锁与超长写入分片
//	go run ./example -wave ':DISP:DATA?'     # 读取 IEEE 488.2 定长块（波形数据）
//
// 退出码：0 成功，1 任一步骤失败。
package main
 
import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go-vxi/vxi11"
)

// 命令行参数
var (
	host     = flag.String("host", "5.5.5.1", "仪器 IP 或主机名")
	device   = flag.String("device", "inst0", "VXI-11 设备名（多数仪器是 inst0）")
	timeout  = flag.Duration("timeout", 5*time.Second, "I/O 超时")
	corePort = flag.Int("port", 0, "DEVICE_CORE 端口；0 表示通过 portmapper 自动发现")
	doLock   = flag.Bool("lock", false, "演示 device_lock / device_unlock")
	doClear  = flag.Bool("clear", false, "演示 device_clear")
	doSRQ    = flag.Bool("srq", false, "演示 interrupt channel：create_intr_chan + device_enable_srq + 用 *OPC 触发一次 SRQ")
	bigWrite = flag.Int("bigwrite", 0, "写入 N 字节的长命令，演示超过 maxRecvSize 时的分片")
	waveCmd  = flag.String("wave", "", "读取二进制块的 SCPI 命令，如 :DISP:DATA?（演示长响应分片拼接）")
	readSTB  = flag.Bool("stb", false, "演示 device_readstb（读状态字节）")
	rawWrite = flag.String("write", "", "直接写入这条命令（不读应答）")
	// 下面两个参数用于排查设备差异：有些仪器不接受分片的 device_write，
	// 有些则对单次读取长度更敏感。
	readSize   = flag.Uint("readsize", 0, "单次 device_read 请求的字节数；0 = 默认 64 KiB")
	writeChunk = flag.Int("writechunk", -1, "device_write 分片大小；0 = 不分片（一次写完），-1 = 跟随 maxRecvSize")
	rawQuery   = queryList{}
	verbose    = flag.Bool("v", false, "打印每一步用到的参数")
)

// queryList 支持 -q 重复出现多次。
type queryList []string

func (q *queryList) String() string { return strings.Join(*q, ",") }

func (q *queryList) Set(v string) error {
	*q = append(*q, v)
	return nil
}

func init() {
	flag.Var(&rawQuery, "q", "要执行的 SCPI 查询，可重复指定（默认 *IDN?）")
}

// step 打印步骤标题，便于对照任务说明的调试步骤。
func step(n int, format string, args ...any) {
	fmt.Printf("\n[步骤 %d] %s\n", n, fmt.Sprintf(format, args...))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n错误: %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

// warn 用于可选的 VXI-11 过程：规范允许设备不实现它们（返回错误码 8），
// 因此只提示不中断演示。实测 RIGOL DS1000Z 系列就不支持 device_readstb。
func warn(format string, args ...any) {
	fmt.Printf("      警告: %s\n", fmt.Sprintf(format, args...))
}

func main() {
	flag.Parse()

	queries := rawQuery
	if len(queries) == 0 {
		queries = []string{"*IDN?"}
	}

	// 步骤 1+2：portmapper 端口发现 + create_link。
	// Dial 内部先连 host:111 查 DEVICE_CORE 端口，再建立 core channel 并发 create_link。
	step(1, "连接 %s（device=%q, timeout=%v）", *host, *device, *timeout)
	var opts []vxi11.Option
	if *corePort > 0 {
		// 规范建议走 portmapper；-port 仅用于设备未跑 portmapper 或调试时锁定端口。
		opts = append(opts, vxi11.WithCorePort(*corePort))
		fmt.Printf("      跳过 portmapper，直接使用端口 %d\n", *corePort)
	}
	if *readSize > 0 {
		opts = append(opts, vxi11.WithReadSize(uint32(*readSize)))
		fmt.Printf("      单次读取长度 = %d 字节\n", *readSize)
	}
	if *writeChunk >= 0 {
		opts = append(opts, vxi11.WithWriteChunkSize(*writeChunk))
		fmt.Printf("      device_write 分片大小 = %d 字节（0 表示不分片）\n", *writeChunk)
	}
	c, err := vxi11.Dial(*host, *device, *timeout, opts...)
	if err != nil {
		hint(err)
		fail("Dial 失败: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭连接时出错: %v\n", err)
			return
		}
		fmt.Println("\n[结束] 已发送 destroy_link 并关闭连接")
	}()

	fmt.Printf("      core channel 端口 = %d\n", c.Port())
	fmt.Printf("      lid = %d\n", c.LinkID())
	fmt.Printf("      maxRecvSize = %d 字节\n", c.MaxRecvSize())
	if c.AbortPort() != 0 {
		fmt.Printf("      abortPort = %d\n", c.AbortPort())
	} else {
		fmt.Println("      abortPort = 无（设备未提供 abort channel）")
	}

	// 步骤 3：Query = device_write + 多次 device_read（自动拼接分片）。
	step(2, "执行 SCPI 查询（write + 分片 read）")
	for _, q := range queries {
		start := time.Now()
		resp, err := c.Query(q)
		if err != nil {
			fail("查询 %q 失败: %v", q, err)
		}
		fmt.Printf("      %-24s -> %q（%v）\n", q, resp, time.Since(start).Round(time.Millisecond))
	}

	// c.WriteString("SAVE:IMAGE \"C:/Temp/screen.png\"")
	// time.Sleep(3 * time.Second)

	// resp, err := c.QueryBytes("FILESystem:READFile \"C:/Temp/screen.png\"", 10*time.Second)
	// if err != nil {
	// 	fail("查询 失败: %v", err)
	// }
	// os.WriteFile("scr.png", resp, 0o644)

	// 直接写一条命令
	if *rawWrite != "" {
		step(3, "写入命令（%d 字节）", len(*rawWrite))
		n, err := c.Write([]byte(*rawWrite), *timeout)
		if err != nil {
			fail("Write 失败: %v", err)
		}
		fmt.Printf("      已写入 %d 字节\n", n)
	}

	// 状态字节
	if *readSTB {
		step(3, "device_readstb：读取状态字节（*STB? 的 VXI-11 版本）")
		stb, err := c.ReadSTB()
		switch {
		case errors.Is(err, vxi11.ErrOperationNotSupported):
			warn("该设备不支持 device_readstb（错误码 8），请改用 SCPI 的 *STB? 查询")
		case err != nil:
			warn("ReadSTB 失败: %v", err)
		default:
			fmt.Printf("      状态字节 = %#02x (%s)\n", stb, strings.Join(decodeSTB(stb), ", "))
		}
	}

	// 加锁
	if *doLock {
		step(4, "device_lock / device_unlock")
		if err := c.Lock(0); err != nil {
			if errors.Is(err, vxi11.ErrDeviceLocked) {
				fail("设备被其它 link 锁定（错误码 11）")
			}
			fail("Lock 失败: %v", err)
		}
		fmt.Println("      加锁成功")
		if err := c.Unlock(); err != nil {
			fail("Unlock 失败: %v", err)
		}
		fmt.Println("      解锁成功")
	}

	// 超长写入分片
	if *bigWrite > 0 {
		step(5, "写入 %d 字节的长命令，演示超过 maxRecvSize 时的分片", *bigWrite)
		// 构造一条合法的 SCPI 命令：:DATA#<n>... 只是示例载荷，真实使用时替换成仪器支持的命令。
		payload := ":SYST:COMM:LAN:DESC '"
		payload += strings.Repeat("A", max(0, *bigWrite-len(payload)-len("'\n")))
		payload += "'\n"
		n, err := c.Write([]byte(payload), *timeout)
		if err != nil {
			fail("Write 失败: %v", err)
		}
		fragments := (n + int(c.MaxRecvSize()) - 1) / int(c.MaxRecvSize())
		fmt.Printf("      已写入 %d 字节，按 maxRecvSize=%d 计算约分 %d 片（仅最后一片带 end 标志）\n",
			n, c.MaxRecvSize(), fragments)
	}

	// 二进制长响应
	if *waveCmd != "" {
		step(6, "读取二进制数据块 %q", *waveCmd)
		cmd := *waveCmd
		if !strings.HasSuffix(cmd, "\n") {
			cmd += "\n"
		}
		data, err := c.QueryBytes(cmd, 20*time.Second)
		if err != nil {
			fail("QueryBytes 失败: %v", err)
		}
		fmt.Printf("      收到 %d 字节\n", len(data))
		if len(data) > 0 && data[0] == '#' {
			body, err := parseDefiniteLengthBlock(data)
			if err != nil {
				fmt.Printf("      IEEE 488.2 定长块解析失败: %v\n", err)
			} else {
				fmt.Printf("      定长块头 %q，数据 %d 字节，前 16 字节: % x\n",
					data[:len(data)-len(body)], len(body), head(body, 16))
			}
			os.WriteFile("screen.jpg", body, 0o644)
		} else {
			fmt.Printf("      前 32 字节: %q\n", head(data, 32))
		}
	}

	// 清缓冲
	if *doClear {
		step(7, "device_clear：清空设备输入/输出缓冲")
		if err := c.Clear(); err != nil {
			fail("Clear 失败: %v", err)
		}
		fmt.Println("      已清空")
	}

	// interrupt channel + SRQ 异步上报
	if *doSRQ {
		step(8, "interrupt channel：设备发生 SRQ 时主动回调（device_intr_srq）")
		got := make(chan []byte, 1)
		c.OnSRQ(func(handle []byte) { got <- handle })
		if err := c.CreateIntrChan(); err != nil {
			fail("CreateIntrChan 失败: %v", err)
		}
		fmt.Println("      interrupt channel 已建立（本机 DEVICE_INTR 服务端已就绪）")
		if err := c.EnableSRQ(true, []byte("demo-handle")); err != nil {
			if errors.Is(err, vxi11.ErrOperationNotSupported) {
				warn("该设备不支持 device_enable_srq（错误码 8）")
				return
			}
			fail("EnableSRQ 失败: %v", err)
		}
		// IEEE 488.2 标准 SRQ 触发序列：操作完成事件 -> ESB -> SRQ。
		if err := c.WriteString("*ESE 1; *SRE 32; *OPC"); err != nil {
			fail("写入 SRQ 触发序列失败: %v", err)
		}
		fmt.Println("      已写入 *ESE 1; *SRE 32; *OPC，等待设备发起 device_intr_srq ...")
		select {
		case h := <-got:
			fmt.Printf("      收到 device_intr_srq，handle = %q\n", h)
		case <-time.After(10 * time.Second):
			warn("10 秒内没有收到 SRQ（部分设备需要真实事件源才会触发）")
		}
		if err := c.WriteString("*SRE 0; *ESE 0"); err != nil {
			warn("收尾写入失败: %v", err)
		}
		if err := c.EnableSRQ(false, nil); err != nil {
			warn("关闭 SRQ 上报失败: %v", err)
		}
		if err := c.DestroyIntrChan(); err != nil {
			warn("DestroyIntrChan 失败: %v", err)
		} else {
			fmt.Println("      interrupt channel 已拆除")
		}
	}

	if *verbose {
		fmt.Printf("\n[调试] %s\n", c)
	}
}

// parseDefiniteLengthBlock 解析 IEEE 488.2 定长块格式：
// '#' <1 位十进制位数 n> <n 位十进制长度> <数据>。
func parseDefiniteLengthBlock(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != '#' {
		return nil, errors.New("不是定长块（首字节不是 '#'）")
	}
	n := int(data[1] - '0')
	if n < 1 || n > 9 {
		return nil, fmt.Errorf("非法长度位数 %d（'#' 后应为 1-9）", n)
	}
	if len(data) < 2+n {
		return nil, fmt.Errorf("长度字段不完整：需要 %d 字节", 2+n)
	}
	size, err := strconv.Atoi(string(data[2 : 2+n]))
	if err != nil {
		return nil, fmt.Errorf("非法长度字段 %q: %w", data[2:2+n], err)
	}
	body := data[2+n:]
	if len(body) < size {
		return nil, fmt.Errorf("数据不完整：声明 %d 字节，实际 %d 字节", size, len(body))
	}
	return body[:size], nil
}

// decodeSTB 把状态字节展开成可读的位含义（IEEE 488.2 标准定义）。
func decodeSTB(stb byte) []string {
	bits := []struct {
		mask byte
		name string
	}{
		{0x01, "bit0"},
		{0x02, "bit1"},
		{0x04, "EAV 错误/事件可用"},
		{0x08, "QUES 问题数据可用"},
		{0x10, "MAV 消息可用"},
		{0x20, "ESB 事件状态位有效"},
		{0x40, "MSS/RQS 请求服务"},
		{0x80, "bit7"},
	}
	var out []string
	for _, b := range bits {
		if stb&b.mask != 0 {
			out = append(out, b.name)
		}
	}
	if len(out) == 0 {
		return []string{"无位被置"}
	}
	return out
}

func head(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// hint 针对常见错误给出排查建议。
func hint(err error) {
	switch {
	case errors.Is(err, vxi11.ErrPortmapper):
		fmt.Fprintln(os.Stderr, "提示: portmapper 未返回 DEVICE_CORE 端口 —— 检查仪器是否启用 VXI-11，"+
			"或改用 -port 直接指定端口")
	case errors.Is(err, vxi11.ErrDeviceNotAccessible):
		fmt.Fprintln(os.Stderr, "提示: 设备名不对 —— 多数示波器是 inst0，可查仪器手册或 VISA 资源串")
	case errors.Is(err, vxi11.ErrDeviceLocked):
		fmt.Fprintln(os.Stderr, "提示: 设备被其它 link 锁定 —— 关掉 NI-MAX/其它上位机软件后重试")
	case errors.Is(err, vxi11.ErrTimeout):
		fmt.Fprintln(os.Stderr, "提示: 网络超时 —— 确认能 ping 通仪器，或加大 -timeout")
	case errors.Is(err, vxi11.ErrConnClosed), errors.Is(err, vxi11.ErrTransport):
		fmt.Fprintln(os.Stderr, "提示: 连接被关闭/传输层错误 —— 检查端口是否被防火墙拦截")
	}
}
