# go-vxi

纯 Go 实现的 **VXI-11（TCP/IP Instrument Protocol）客户端**，用于通过 LAN 控制示波器、信号源、
万用表等支持 VXI-11 的仪器。

- **零第三方依赖、无 cgo**：从 TCP 到 SCPI 全部自己实现，可直接交叉编译。
- 自己实现 ONC RPC（[RFC 5531](https://www.rfc-editor.org/rfc/rfc5531)）、XDR（[RFC 4506](https://www.rfc-editor.org/rfc/rfc4506)）
  与 portmapper 端口发现。
- 写自动分片、读自动拼接到结束，长响应不会被截断。
- 错误码可用 `errors.Is` / `errors.As` 判定。
- 内置内存假仪器，无硬件即可端到端测试。

对应规范：**VXI-11 Rev 1.0**（`vxi-11.pdf`）。

## 目录结构

```
go-vxi/
├── go.mod
├── vxi11/                  客户端包
│   ├── client.go           对外 API：Client、Dial、Option
│   ├── protocol.go         VXI-11 过程号与结构体编解码
│   ├── rpc.go              ONC RPC over TCP + 记录标记
│   ├── xdr.go              XDR 编解码器
│   ├── intr.go             interrupt channel（DEVICE_INTR 服务端，SRQ 上报）
│   ├── errors.go           设备错误码与本地哨兵错误
│   ├── mockdevice_test.go  内存假仪器（portmapper + core + abort + intr 回连）
│   └── integration_test.go 真机集成测试（build tag: integration）
├── example/                命令行演示程序
└── docs/api.html           接口说明文档（HTML，浏览器打开）
```

## 快速开始

```go
package main

import (
	"fmt"
	"log"
	"time"

	"go-vxi/vxi11"
)

func main() {
	// portmapper 查端口 -> 建 core channel -> create_link
	c, err := vxi11.Dial("5.5.5.1", "inst0", 5*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close() // 发送 destroy_link 并关闭连接

	idn, err := c.Query("*IDN?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(idn)
	// RIGOL TECHNOLOGIES,DS1104Z,DS1ZA212606163,00.04.04.SP4
}
```

## 演示程序

```bash
# 最小演示：建链 + *IDN?
go run ./example -host 5.5.5.1

# 查询多条命令 + 读状态字节 + 读屏幕截图（长响应分片拼接）
go run ./example -host 5.5.5.1 -q "*IDN?" -q ":CHAN1:SCAL?" -stb -wave ":DISP:DATA?"

# 演示锁与清设备
go run ./example -host 5.5.5.1 -lock -clear

# 演示 SRQ 异步上报（interrupt channel + device_enable_srq + *OPC 触发）
go run ./example -host 5.5.5.1 -srq

# 分片写入：写 3006 字节（超过该机 maxRecvSize=1500）
go run ./example -host 5.5.5.1 -bigwrite 3006 -v
```

主要参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-host` | `5.5.5.1` | 仪器 IP 或主机名 |
| `-device` | `inst0` | VXI-11 设备名 |
| `-timeout` | `5s` | I/O 超时 |
| `-port` | `0` | 非 0 时跳过 portmapper，直连该 core 端口 |
| `-q` | `*IDN?` | 要执行的 SCPI 查询，可重复指定 |
| `-write` | 空 | 直接写入这条命令（不读应答） |
| `-wave` | 空 | 读取二进制块的 SCPI 命令，如 `:DISP:DATA?` |
| `-stb` / `-lock` / `-clear` | `false` | 演示 `device_readstb` / 锁 / `device_clear` |
| `-srq` | `false` | 演示 interrupt channel：建立通道、使能 SRQ、用 `*OPC` 触发一次 SRQ 回调 |
| `-bigwrite` | `0` | 写入 N 字节的长命令，演示分片 |
| `-readsize` | `0` | 覆盖单次 `device_read` 请求长度；0 = 默认 64 KiB |
| `-writechunk` | `-1` | 写入分片大小；`0` = 不分片，`-1` = 跟随 `maxRecvSize` |
| `-v` | `false` | 打印每一步的详细参数 |

## 已实现的过程

| VXI-11 过程 | proc | API | 状态 |
| --- | --- | --- | --- |
| `create_link` | 10 | `Dial` | ✅ |
| `device_write` | 11 | `Write` / `WriteString` / `Query*` | ✅ |
| `device_read` | 12 | `Read` / `ReadAll` / `Query*` | ✅ |
| `device_readstb` | 13 | `ReadSTB` | ✅ |
| `device_trigger` | 14 | `Trigger` | ✅ |
| `device_clear` | 15 | `Clear` | ✅ |
| `device_remote` / `device_local` | 16 / 17 | `Remote` / `Local` | ✅ |
| `device_lock` / `device_unlock` | 18 / 19 | `Lock` / `Unlock` | ✅ |
| `device_enable_srq` | 20 | `EnableSRQ` | ✅ |
| `device_docmd` | 22 | `Docmd` | ✅ |
| `destroy_link` | 23 | `Close` | ✅ |
| `create_intr_chan` / `destroy_intr_chan` | 25 / 26 | `CreateIntrChan` / `DestroyIntrChan` | ✅ |
| `device_abort` | — (async) | `Abort` | ✅ |
| `device_intr_srq` | — (intr，设备→客户端) | `OnSRQ` 回调 | ✅ |

至此 VXI-11 Rev 1.0 定义的全部 14 个过程均已实现。
interrupt channel 的完整用法（SRQ 异步上报）：

```go
c.OnSRQ(func(handle []byte) {
    fmt.Printf("设备请求服务，handle=%q\n", handle)
})
if err := c.CreateIntrChan(); err != nil { /* 设备不支持时返回错误码 8/6 */ }
c.EnableSRQ(true, []byte("my-handle"))
// ... 触发 SRQ（如 *ESE 1; *SRE 32; *OPC）后，设备会主动回调 OnSRQ
c.DestroyIntrChan()
```

## 测试

```bash
# 单元测试（无需硬件，走内存假仪器）
go test ./...
go test -cover ./vxi11/

# 静态检查
gofmt -l .
go vet ./...
go vet -tags integration ./...

# 集成测试（对真实仪器，默认跳过）
go test -tags integration -v ./vxi11/ -host 5.5.5.1
# 或
VXI11_HOST=5.5.5.1 VXI11_DEVICE=inst0 go test -tags integration -v ./vxi11/
```

集成测试用 build tag 隔离，**不指定 `-host` 也不设 `VXI11_HOST` 时全部自动 Skip**，
不会误连仪器。可用的环境变量：

| 变量 | 说明 |
| --- | --- |
| `VXI11_HOST` / `VXI11_DEVICE` | 仪器地址 / 设备名（默认 inst0） |
| `VXI11_LONG_CMD` | 长响应测试命令（默认 `:DISP:DATA?`，不支持时自动退到泰克截图路线） |
| `VXI11_ERR_CMD` | 错误队列查询命令（默认先试 `:SYST:ERR?`，超时自动退到泰克的 `ALLEv?`） |

## 两份文档

- **[docs/api.html](docs/api.html)** — 完整的接口说明：函数、变量、常量、错误码、协议表、真机实测记录。
- **`go doc go-vxi/vxi11`** — 每个导出符号的注释（含用法与注意事项）。

## 实测提示

### RIGOL DS1000Z 系列（DS1104Z，`5.5.5.1`）

- portmapper 返回 core 端口 **618**，`maxRecvSize = 1500`，`abortPort = 619`。
- `device_readstb` 与 `device_abort` 都返回错误码 **8（operation not supported）**，改用 SCPI 的 `*STB?`。
- 单次 `device_write` 超过 `maxRecvSize` 会被拒绝（错误码 **5**），**必须分片**。
- 读 1.1 MB 的 `:DISP:DATA?`：请求长度用 `maxRecvSize`（1500）要 72 秒，
  用 64 KiB 只要 3.4 秒——这是本包默认 `readSize = 64 KiB` 的原因。

### Tektronix MSO58（`10.42.71.185`，本仓库当前调试设备）

- portmapper 返回 core 端口 **7000**，`maxRecvSize = 4096`，`abortPort = 7001`。
- `device_readstb`、锁、`device_clear`、`device_trigger` 均正常；
  interrupt channel（`create_intr_chan` + `device_enable_srq` + SRQ 回调）实测可用。
- **abort channel 是坏的**：`device_abort` 在无在途操作时返回错误码 4
  （invalid link identifier，规范要求此值仅在 lid 不匹配时返回），
  有在途 `device_read` 时则根本不应答。需要中断时建议直接重开连接。
- 不支持 `:SYST:ERR?`（查询读超时，错误码 15），错误队列改用泰克的 `ALLEv?`。
- 不支持 `:DISP:DATA?`；屏幕截图用 `SAVe:IMAGe "C:/Temp/screen.png"`
  再 `FILESystem:READFile 'C:/Temp/screen.png'`（返回的是裸 PNG，不是 IEEE 488.2 定长块）。
- 不支持的查询命令**不会产生任何响应**，`device_read` 会一直等到设备侧超时
  才返回错误码 15——新接入仪器时先用 `*IDN?` 验证命令拼写。

## License

未指定。
