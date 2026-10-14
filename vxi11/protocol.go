package vxi11

import (
	"fmt"
	"strings"
)

// 本文件是 VXI-11 设备侧数据结构的 XDR 编解码，结构定义与字段顺序
// 严格对应规范 Section C.1（vxi-11.pdf）。

// DEVICE_CORE version 1 的过程号（规范 Section C.1）。
const (
	procCreateLink      uint32 = 10
	procDeviceWrite     uint32 = 11
	procDeviceRead      uint32 = 12
	procDeviceReadStb   uint32 = 13
	procDeviceTrigger   uint32 = 14
	procDeviceClear     uint32 = 15
	procDeviceRemote    uint32 = 16
	procDeviceLocal     uint32 = 17
	procDeviceLock      uint32 = 18
	procDeviceUnlock    uint32 = 19
	procDeviceEnableSrq uint32 = 20
	procDeviceDocmd     uint32 = 22
	procDestroyLink     uint32 = 23
	procCreateIntrChan  uint32 = 25
	procDestroyIntrChan uint32 = 26

	// DEVICE_ASYNC version 1（abort channel）。
	procDeviceAbort uint32 = 1

	// DEVICE_INTR version 1（interrupt channel，设备 -> 客户端方向的调用）。
	procDeviceIntrSrq uint32 = 30
)

// procName 返回过程名，用于错误信息。
func procName(proc uint32) string {
	switch proc {
	case procCreateLink:
		return "create_link"
	case procDeviceWrite:
		return "device_write"
	case procDeviceRead:
		return "device_read"
	case procDeviceReadStb:
		return "device_readstb"
	case procDeviceTrigger:
		return "device_trigger"
	case procDeviceClear:
		return "device_clear"
	case procDeviceRemote:
		return "device_remote"
	case procDeviceLocal:
		return "device_local"
	case procDeviceLock:
		return "device_lock"
	case procDeviceUnlock:
		return "device_unlock"
	case procDeviceEnableSrq:
		return "device_enable_srq"
	case procDeviceDocmd:
		return "device_docmd"
	case procDestroyLink:
		return "destroy_link"
	case procCreateIntrChan:
		return "create_intr_chan"
	case procDestroyIntrChan:
		return "destroy_intr_chan"
	case procDeviceAbort:
		return "device_abort"
	case procDeviceIntrSrq:
		return "device_intr_srq"
	default:
		return fmt.Sprintf("proc(%d)", proc)
	}
}

// Flags 是设备操作标志位 Device_Flags（规范 Figure B.16）。
type Flags uint32

const (
	// FlagWaitLock（bit0，waitlock）：device_lock 时若锁被占用则等待，
	// 而不是立刻返回错误码 11。
	FlagWaitLock Flags = 1 << 0
	// FlagEnd（bit3，end）：device_write 专用，置位表示本次写入的最后一个字节带 END 指示。
	FlagEnd Flags = 1 << 3
	// FlagTermChrSet（bit7，termchrset）：device_read 专用，置位表示使用 TermChar 作为终止符。
	FlagTermChrSet Flags = 1 << 7
)

func (f Flags) String() string {
	if f == 0 {
		return "0"
	}
	var parts []string
	if f&FlagWaitLock != 0 {
		parts = append(parts, "waitlock")
	}
	if f&FlagEnd != 0 {
		parts = append(parts, "end")
	}
	if f&FlagTermChrSet != 0 {
		parts = append(parts, "termchrset")
	}
	if rest := f &^ (FlagWaitLock | FlagEnd | FlagTermChrSet); rest != 0 {
		parts = append(parts, fmt.Sprintf("%#x", uint32(rest)))
	}
	return strings.Join(parts, "|")
}

// ReadReason 是 device_read 返回的 reason 位（规范 Table B.7）。
type ReadReason int32

const (
	// ReasonReqCnt（bit0）：已读满请求的 requestSize 字节。
	ReasonReqCnt ReadReason = 1 << 0
	// ReasonChr（bit1）：读到了 TermChar 终止符。
	ReasonChr ReadReason = 1 << 1
	// ReasonEnd（bit2）：读到了设备发出的 END 指示（SCPI 应答结束的常见情形）。
	ReasonEnd ReadReason = 1 << 2
)

// Has 判断 reason 是否包含指定位。
func (r ReadReason) Has(f ReadReason) bool { return r&f != 0 }

func (r ReadReason) String() string {
	if r == 0 {
		return "0"
	}
	var parts []string
	if r.Has(ReasonReqCnt) {
		parts = append(parts, "REQCNT")
	}
	if r.Has(ReasonChr) {
		parts = append(parts, "CHR")
	}
	if r.Has(ReasonEnd) {
		parts = append(parts, "END")
	}
	if rest := r &^ (ReasonReqCnt | ReasonChr | ReasonEnd); rest != 0 {
		parts = append(parts, fmt.Sprintf("%#x", int32(rest)))
	}
	return strings.Join(parts, "|")
}

// ---------------------------------------------------------------------------
// create_link (proc 10)
// ---------------------------------------------------------------------------

// createLinkParms 对应 Create_LinkParms。
type createLinkParms struct {
	clientID    int32  // 任意标识，供设备端调试/追踪用
	lockDevice  bool   // 是否在建立 link 时尝试加锁
	lockTimeout uint32 // 等锁超时（毫秒）
	device      string // 设备名，如 "inst0"
}

func (p createLinkParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.clientID)
	e.boolean(p.lockDevice)
	e.uint32(p.lockTimeout)
	e.string(p.device)
	return e.bytes()
}

// createLinkResp 对应 Create_LinkResp。
type createLinkResp struct {
	err         int32
	lid         int32  // 连接标识（Link ID），后续所有操作都要带
	abortPort   uint16 // abort channel 端口，0 表示设备不提供
	maxRecvSize uint32 // 单次 device_write 的最大字节数（规范要求至少 1024）
}

// parseCreateLinkResp 解析 Create_LinkResp。
//
// 严格按规范，无论 error 是否为 0，设备都应当发送完整的 4 个字段；
// 但确实存在只回一个 Device_Error 的实现，因此 error != 0 时不再强制要求后续字段存在
// （此时 lid/abortPort/maxRecvSize 本来也没有意义）。
func parseCreateLinkResp(d *xdrDecoder) createLinkResp {
	r := createLinkResp{err: d.int32()}
	if r.err != 0 || d.error() != nil {
		return r
	}
	r.lid = d.int32()
	r.abortPort = d.uint16()
	r.maxRecvSize = d.uint32()
	return r
}

// ---------------------------------------------------------------------------
// device_write (proc 11)
// ---------------------------------------------------------------------------

// deviceWriteParms 对应 Device_WriteParms。
type deviceWriteParms struct {
	lid         int32
	ioTimeout   uint32 // 设备侧 I/O 超时（毫秒），0 表示不设超时
	lockTimeout uint32 // 设备侧等锁超时（毫秒）
	flags       Flags  // bit3=end
	data        []byte
}

func (p deviceWriteParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.uint32(p.ioTimeout)
	e.uint32(p.lockTimeout)
	e.uint32(uint32(p.flags))
	e.opaque(p.data)
	return e.bytes()
}

// deviceWriteResp 对应 Device_WriteResp。
type deviceWriteResp struct {
	err  int32
	size uint32 // 实际写入设备的字节数
}

func parseDeviceWriteResp(d *xdrDecoder) deviceWriteResp {
	return deviceWriteResp{err: d.int32(), size: d.uint32()}
}

// ---------------------------------------------------------------------------
// device_read (proc 12)
// ---------------------------------------------------------------------------

// deviceReadParms 对应 Device_ReadParms。
type deviceReadParms struct {
	lid         int32
	requestSize uint32 // 本次最多读取的字节数
	ioTimeout   uint32
	lockTimeout uint32
	flags       Flags // bit7=termchrset
	termChar    byte
}

func (p deviceReadParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.uint32(p.requestSize)
	e.uint32(p.ioTimeout)
	e.uint32(p.lockTimeout)
	e.uint32(uint32(p.flags))
	e.char(p.termChar)
	return e.bytes()
}

// deviceReadResp 对应 Device_ReadResp。
type deviceReadResp struct {
	err    int32
	reason ReadReason
	data   []byte
}

func parseDeviceReadResp(d *xdrDecoder) deviceReadResp {
	return deviceReadResp{
		err:    d.int32(),
		reason: ReadReason(d.int32()),
		data:   d.opaque(maxOpaqueLen),
	}
}

// ---------------------------------------------------------------------------
// device_readstb (proc 13)
// ---------------------------------------------------------------------------

// deviceReadStbResp 对应 Device_ReadStbResp。
type deviceReadStbResp struct {
	err int32
	stb byte // 状态字节（Status Byte）
}

func parseDeviceReadStbResp(d *xdrDecoder) deviceReadStbResp {
	return deviceReadStbResp{err: d.int32(), stb: d.char()}
}

// ---------------------------------------------------------------------------
// Device_GenericParms：proc 14/15/16/17/20 共用
// ---------------------------------------------------------------------------

// deviceGenericParms 对应 Device_GenericParms。
// 注意字段顺序是 lid, flags, lock_timeout, io_timeout（与 Write/Read 不同）。
type deviceGenericParms struct {
	lid         int32
	flags       Flags
	lockTimeout uint32
	ioTimeout   uint32
}

func (p deviceGenericParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.uint32(uint32(p.flags))
	e.uint32(p.lockTimeout)
	e.uint32(p.ioTimeout)
	return e.bytes()
}

// ---------------------------------------------------------------------------
// device_lock (proc 18) / device_unlock (proc 19)
// ---------------------------------------------------------------------------

// deviceLockParms 对应 Device_LockParms。
type deviceLockParms struct {
	lid         int32
	flags       Flags // bit0=waitlock
	lockTimeout uint32
}

func (p deviceLockParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.uint32(uint32(p.flags))
	e.uint32(p.lockTimeout)
	return e.bytes()
}

// deviceLink 对应规范里的 Device_Link（一个 int32 的 lid），
// 用于 destroy_link / device_unlock / device_abort 的参数。
type deviceLink struct {
	lid int32
}

func (p deviceLink) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	return e.bytes()
}

// ---------------------------------------------------------------------------
// device_enable_srq (proc 20)
// ---------------------------------------------------------------------------

// deviceEnableSrqParms 对应 Device_EnableSrqParms。
type deviceEnableSrqParms struct {
	lid    int32
	enable bool
	handle []byte // 中断通道句柄，规范限制最大 40 字节
}

func (p deviceEnableSrqParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.boolean(p.enable)
	e.opaque(p.handle)
	return e.bytes()
}

// ---------------------------------------------------------------------------
// device_docmd (proc 22)
// ---------------------------------------------------------------------------

// deviceDocmdParms 对应 Device_DocmdParms。
// 字段顺序与规范一致：lid, flags, io_timeout, lock_timeout, cmd,
// network_order, datasize, data_in（注意与 Device_GenericParms 不同）。
type deviceDocmdParms struct {
	lid          int32
	flags        Flags
	ioTimeout    uint32
	lockTimeout  uint32
	cmd          int32
	networkOrder bool   // 客户端字节序是否为网络序（大端）
	datasize     int32  // data_in 里单个数据元素的字节数（供设备端按需字节交换）
	data         []byte // docmd 数据参数
}

func (p deviceDocmdParms) marshal() []byte {
	e := &xdrEncoder{}
	e.int32(p.lid)
	e.uint32(uint32(p.flags))
	e.uint32(p.ioTimeout)
	e.uint32(p.lockTimeout)
	e.int32(p.cmd)
	e.boolean(p.networkOrder)
	e.int32(p.datasize)
	e.opaque(p.data)
	return e.bytes()
}

// deviceDocmdResp 对应 Device_DocmdResp。
type deviceDocmdResp struct {
	err  int32
	data []byte
}

func parseDeviceDocmdResp(d *xdrDecoder) deviceDocmdResp {
	return deviceDocmdResp{err: d.int32(), data: d.opaque(maxOpaqueLen)}
}

// ---------------------------------------------------------------------------
// create_intr_chan (proc 25) / destroy_intr_chan (proc 26)
// ---------------------------------------------------------------------------

// Device_AddrFamily（规范 B.6.12）：interrupt channel 用的传输协议。
const (
	// addrFamilyTCP 对应 DEVICE_TCP（enum 第一个值，编码为 0）。
	addrFamilyTCP uint32 = 0
	// addrFamilyUDP 对应 DEVICE_UDP（enum 第二个值，编码为 1）。
	addrFamilyUDP uint32 = 1
)

// deviceRemoteFunc 对应 Device_RemoteFunc（proc 25 的参数）：
// 告诉设备到哪里（IP/端口/程序号）建立 interrupt channel。
//
// 注意：XDR 里没有 2 字节类型，hostPort（unsigned short）在线上也占 4 字节。
type deviceRemoteFunc struct {
	hostAddr   uint32 // 中断通道监听的 IPv4 地址（网络字节序的 4 字节按 uint32 解释）
	hostPort   uint32 // 中断通道监听的端口
	progNum    uint32 // 必须是 deviceIntrProg（395185 = 0x0607B1），否则错误码 8
	progVers   uint32 // 必须是 1，否则错误码 8
	progFamily uint32 // addrFamilyTCP / addrFamilyUDP
}

func (p deviceRemoteFunc) marshal() []byte {
	e := &xdrEncoder{}
	e.uint32(p.hostAddr)
	e.uint32(p.hostPort)
	e.uint32(p.progNum)
	e.uint32(p.progVers)
	e.uint32(p.progFamily)
	return e.bytes()
}
