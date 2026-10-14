package vxi11

import (
	"errors"
	"fmt"
)

// ErrorCode 是 VXI-11 设备错误码（规范 Table B.2）。
//
// 它本身实现了 error 接口，因此可以直接和 errors.Is 一起使用：
//
//	if errors.Is(err, vxi11.ErrDeviceLocked) { ... }
type ErrorCode int32

// VXI-11 设备错误码（规范 Table B.2）。未列出的取值以 ErrorCode 原样返回。
const (
	ErrNoError               ErrorCode = 0  // no error
	ErrSyntax                ErrorCode = 1  // syntax error
	ErrDeviceNotAccessible   ErrorCode = 3  // device not accessible
	ErrInvalidLinkID         ErrorCode = 4  // invalid link identifier
	ErrParameter             ErrorCode = 5  // parameter error
	ErrChannelNotEstablished ErrorCode = 6  // channel not established
	ErrOperationNotSupported ErrorCode = 8  // operation not supported
	ErrOutOfResources        ErrorCode = 9  // out of resources
	ErrDeviceLocked          ErrorCode = 11 // device locked by another link
	ErrNoLockHeld            ErrorCode = 12 // no lock held by this link
	ErrIOTimeout             ErrorCode = 15 // I/O timeout
	ErrIOError               ErrorCode = 17 // I/O error
	ErrInvalidAddress        ErrorCode = 21 // invalid address
	ErrAbort                 ErrorCode = 23 // abort
	ErrChannelEstablished    ErrorCode = 29 // channel already established
)

// errorCodeNames 是错误码到规范中英文名称的映射。
var errorCodeNames = map[ErrorCode]string{
	ErrNoError:               "no error",
	ErrSyntax:                "syntax error",
	ErrDeviceNotAccessible:   "device not accessible",
	ErrInvalidLinkID:         "invalid link identifier",
	ErrParameter:             "parameter error",
	ErrChannelNotEstablished: "channel not established",
	ErrOperationNotSupported: "operation not supported",
	ErrOutOfResources:        "out of resources",
	ErrDeviceLocked:          "device locked by another link",
	ErrNoLockHeld:            "no lock held by this link",
	ErrIOTimeout:             "I/O timeout",
	ErrIOError:               "I/O error",
	ErrInvalidAddress:        "invalid address",
	ErrAbort:                 "abort",
	ErrChannelEstablished:    "channel already established",
}

// String 返回规范中该错误码的名称，未知码返回 "error code N"。
func (c ErrorCode) String() string {
	if s, ok := errorCodeNames[c]; ok {
		return s
	}
	return fmt.Sprintf("error code %d", int32(c))
}

// Error 实现 error 接口。
func (c ErrorCode) Error() string {
	return fmt.Sprintf("device error %d (%s)", int32(c), c.String())
}

// AsError 把错误码转换为 error：0 返回 nil，其余返回 ErrorCode 自身。
func (c ErrorCode) AsError() error {
	if c == ErrNoError {
		return nil
	}
	return c
}

// DeviceError 表示设备端在响应中返回的错误（VXI-11 各响应结构里的 error 字段）。
//
// 它是唯一会携带"哪个 RPC 过程失败"信息的错误类型，
// DeviceError.Unwrap 返回对应的 ErrorCode，因此：
//
//	errors.Is(err, vxi11.ErrDeviceLocked)     // 设备被其它 link 锁定
//	var de *vxi11.DeviceError
//	errors.As(err, &de)                       // 取出 op / code
type DeviceError struct {
	Op   string    // 触发错误的 RPC 过程名，如 "device_write"
	Code ErrorCode // 设备返回的错误码
}

// Error 实现 error 接口。
func (e *DeviceError) Error() string {
	if e.Op == "" {
		return e.Code.Error()
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Code.Error())
}

// Unwrap 让 errors.Is/errors.As 能识别出具体的 ErrorCode。
func (e *DeviceError) Unwrap() error { return e.Code }

// deviceErr 把设备响应里的错误码转换为 error；0 返回 nil。
func deviceErr(op string, code int32) error {
	if code == 0 {
		return nil
	}
	return &DeviceError{Op: op, Code: ErrorCode(code)}
}

// AsDeviceError 从 err 中提取 *DeviceError；若 err 不是设备错误，ok 为 false。
func AsDeviceError(err error) (de *DeviceError, ok bool) {
	ok = errors.As(err, &de)
	return de, ok
}

// 本地（非设备返回）错误。这些错误码不出现在 VXI-11 报文里，
// 由本包的传输层/XDR 层产生，同样支持 errors.Is。
var (
	// ErrTransport 表示 TCP 传输层错误（连接被重置、写入失败等）。
	ErrTransport = errors.New("vxi11: transport error")
	// ErrTimeout 表示网络层超时（socket 读写截止时间到）。
	// 注意与设备返回的错误码 15（ErrIOTimeout）区分：后者说明是设备自己超时。
	ErrTimeout = errors.New("vxi11: network timeout")
	// ErrConnClosed 表示对端关闭了连接。
	ErrConnClosed = errors.New("vxi11: connection closed by peer")
	// ErrConnBroken 表示该 RPC 通道已不可用（曾经发生过传输层错误）。
	// 出现后所有后续调用都会返回这个错误，调用方应当重新 Dial。
	ErrConnBroken = errors.New("vxi11: connection is broken")
	// ErrProtocol 表示 RPC/XDR 报文格式错误（xid 不匹配、报文过短等）。
	ErrProtocol = errors.New("vxi11: RPC protocol error")
	// ErrProgramUnavailable 表示服务端未注册该 RPC 程序。
	ErrProgramUnavailable = errors.New("vxi11: RPC program unavailable")
	// ErrProgramMismatch 表示服务端不支持请求的版本。
	ErrProgramMismatch = errors.New("vxi11: RPC program version mismatch")
	// ErrProcedureUnavailable 表示服务端不支持该 RPC 过程。
	ErrProcedureUnavailable = errors.New("vxi11: RPC procedure unavailable")
	// ErrGarbageArgs 表示服务端无法解析参数。
	ErrGarbageArgs = errors.New("vxi11: RPC garbage arguments")
	// ErrSystemError 表示服务端内部错误。
	ErrSystemError = errors.New("vxi11: RPC system error")
	// ErrRPCMismatch 表示 RPC 版本协商失败。
	ErrRPCMismatch = errors.New("vxi11: RPC version mismatch")
	// ErrAuthError 表示 RPC 认证被拒绝。
	ErrAuthError = errors.New("vxi11: RPC authentication error")
	// ErrPortmapper 表示通过 portmapper 查询 DEVICE_CORE 端口失败（未注册等）。
	ErrPortmapper = errors.New("vxi11: portmapper lookup failed")
	// ErrResponseTooLarge 表示响应超过 WithMaxResponseBytes 或调用方给出的长度上限。
	ErrResponseTooLarge = errors.New("vxi11: response too large")
	// ErrShortWrite 表示设备报告的写入字节数少于本包提交的字节数。
	ErrShortWrite = errors.New("vxi11: short device_write")
	// ErrClosed 表示 Client 已经 Close，不能继续使用。
	ErrClosed = errors.New("vxi11: client closed")
	// ErrNoAbortChannel 表示设备没有提供 abort 端口，无法执行 Abort。
	ErrNoAbortChannel = errors.New("vxi11: no abort channel")
	// ErrNotImplemented 表示本包尚未实现某个功能。
	//
	// 保留：早期版本中 device_docmd / interrupt channel 会返回它，
	// 现在两者都已实现，目前没有导出函数再返回该错误。
	ErrNotImplemented = errors.New("vxi11: not implemented")
)

// IsTimeout 判断 err 是否属于超时：既是网络层超时（ErrTimeout），
// 也包括设备返回的错误码 15（ErrIOTimeout）。
func IsTimeout(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrIOTimeout)
}

// IsDeviceLocked 判断 err 是否为"设备被其它 link 锁定"（错误码 11）。
func IsDeviceLocked(err error) bool { return errors.Is(err, ErrDeviceLocked) }
