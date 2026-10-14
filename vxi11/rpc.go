package vxi11

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// 本文件实现 ONC RPC（Sun RPC，RFC 5531）在 TCP 上的最小可用子集：
// 调用消息构造、记录标记（record marking）分片、应答解析，以及 xid 校验。
// 认证一律使用 AUTH_NULL（VXI-11 不使用 RPC 认证）。

// ONC RPC 消息类型。
const (
	msgTypeCall  = 0
	msgTypeReply = 1
)

// RPC 应答状态。
const (
	replyStatAccepted = 0
	replyStatDenied   = 1
)

// MSG_DENIED 的拒绝原因。
const (
	rejectStatRPCMismatch = 0
	rejectStatAuthError   = 1
)

// MSG_ACCEPTED 的接受状态。
const (
	acceptStatSuccess      = 0
	acceptStatProgUnavail  = 1
	acceptStatProgMismatch = 2
	acceptStatProcUnavail  = 3
	acceptStatGarbageArgs  = 4
	acceptStatSystemErr    = 5
)

// 认证方式：AUTH_NULL。
const authNull = 0

// 记录标记（RFC 5531 §11）：每个消息由若干分片组成，
// 每个分片前有 4 字节头：最高位=1 表示"最后一片"，低 31 位是本片长度。
const (
	lastFragmentFlag   = 0x80000000
	fragmentLenMask    = 0x7fffffff
	defaultMaxFragment = 1 << 20 // 单个分片最大 1 MiB
)

// RPC 版本，固定为 2。
const rpcVersion = 2

// VXI-11 程序号与版本（规范 Table B.3）。
const (
	// portmapper（rpcbind）程序号/版本，与 ONC RPC 规范一致。
	portmapperProg = 100000
	portmapperVers = 2
	// PMAPPROC_GETPORT：根据 (prog, vers, prot) 查询端口。
	pmProcGetPort = 3
	// PMAPPROC_NULL：空过程，可用于探测 portmapper 是否在线。
	pmProcNull = 0

	// Device_Core：VXI-11 主通道（core channel）程序号。
	deviceCoreProg = 0x0607AF
	deviceCoreVers = 1
	// Device_Async：abort channel 程序号。
	deviceAsyncProg = 0x0607B0
	deviceAsyncVers = 1
	// Device_Intr：中断（SRQ）通道程序号，本包暂未实现。
	deviceIntrProg = 0x0607B1
	deviceIntrVers = 1

	// IPPROTO_TCP，传给 portmapper 的协议号。
	ipProtoTCP = 6
)

// rpcConn 是一条 ONC RPC over TCP 连接（core channel 或 abort channel）。
//
// 它不做并发保护：调用方（Client）用互斥锁保证同一条连接上同一时刻只有一个在途调用。
// 一旦发生传输层错误，连接上剩下的字节流已经无法对齐，
// 因此会置 broken 并让后续调用直接失败，提示调用方重新 Dial。
type rpcConn struct {
	conn      net.Conn
	prog      uint32
	vers      uint32
	maxFrag   int // 发送时单个分片的最大字节数
	maxRecord int // 接收时允许的最大消息总字节数

	nextXID uint32
	broken  error
}

func newRPCConn(conn net.Conn, prog, vers uint32) *rpcConn {
	return &rpcConn{
		conn:      conn,
		prog:      prog,
		vers:      vers,
		maxFrag:   defaultMaxFragment,
		maxRecord: maxRecordBytes,
	}
}

func (r *rpcConn) close() error {
	if r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

func (r *rpcConn) remoteAddr() net.Addr {
	if r.conn == nil {
		return nil
	}
	return r.conn.RemoteAddr()
}

// fail 包装传输层/协议层错误、把连接标记为不可用，并返回包装后的错误。
func (r *rpcConn) fail(err error) error {
	wrapped := wrapNetError(err)
	if r.broken == nil {
		r.broken = errors.Join(ErrConnBroken, wrapped)
	}
	return wrapped
}

// call 执行一次 RPC 调用：编码调用头 + args，发送，读取应答，
// 校验 xid 与应答状态后返回指向"结果区"的解码器。
//
// ioTimeout <= 0 表示不设置 socket 截止时间。
func (r *rpcConn) call(proc uint32, args []byte, ioTimeout time.Duration) (*xdrDecoder, error) {
	if r.broken != nil {
		return nil, r.broken
	}
	if r.conn == nil {
		return nil, ErrConnBroken
	}

	r.nextXID++
	if r.nextXID == 0 {
		r.nextXID = 1
	}
	xid := r.nextXID

	e := &xdrEncoder{}
	e.uint32(xid) // xid
	e.uint32(msgTypeCall)
	e.uint32(rpcVersion)
	e.uint32(r.prog)
	e.uint32(r.vers)
	e.uint32(proc)
	// 凭据：AUTH_NULL，长度为 0。
	e.uint32(authNull)
	e.uint32(0)
	// 校验器：AUTH_NULL，长度为 0。
	e.uint32(authNull)
	e.uint32(0)
	msg := append(e.bytes(), args...)

	deadline, ok := deadlineFor(ioTimeout)
	dl := deadline
	if !ok {
		dl = time.Time{}
	}
	if err := r.conn.SetDeadline(dl); err != nil {
		return nil, r.fail(fmt.Errorf("设置 socket 截止时间失败: %w", err))
	}

	if err := writeRecord(r.conn, msg, r.maxFrag); err != nil {
		return nil, r.fail(err)
	}
	reply, err := readRecord(r.conn, r.maxRecord)
	if err != nil {
		return nil, r.fail(err)
	}

	d, err := parseReply(reply, xid)
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			// 报文错乱说明字节流已经对不齐，连接不能再用了。
			return nil, r.fail(err)
		}
		return nil, err
	}
	return d, nil
}

// writeRecord 把 payload 按记录标记切分成若干分片写入 w。
//
// 为了减少系统调用，所有分片（含分片头）会先拼进一个缓冲区再一次性写出；
// 最后一片的分片头带"最后一片"标志。
func writeRecord(w io.Writer, payload []byte, maxFrag int) error {
	if maxFrag <= 0 {
		maxFrag = defaultMaxFragment
	}
	buf := make([]byte, 0, len(payload)+4*(len(payload)/maxFrag+2))
	rest := payload
	for {
		n := len(rest)
		if n > maxFrag {
			n = maxFrag
		}
		last := n == len(rest)
		v := uint32(n)
		if last {
			v |= lastFragmentFlag
		}
		buf = binary.BigEndian.AppendUint32(buf, v)
		buf = append(buf, rest[:n]...)
		rest = rest[n:]
		if last {
			break
		}
	}
	_, err := w.Write(buf)
	return err
}

// readRecord 读取一个完整消息（可能由多个分片组成），把各分片拼接后返回。
// maxTotal 限制消息总长度，防止对端声称一个超大长度导致内存耗尽。
func readRecord(r io.Reader, maxTotal int) ([]byte, error) {
	var out []byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		v := binary.BigEndian.Uint32(hdr[:])
		n := int(v & fragmentLenMask)
		last := v&lastFragmentFlag != 0
		if n < 0 || len(out)+n > maxTotal {
			return nil, fmt.Errorf("%w: 记录长度 %d 超出上限 %d", ErrProtocol, len(out)+n, maxTotal)
		}
		if n > 0 {
			start := len(out)
			out = append(out, make([]byte, n)...)
			if _, err := io.ReadFull(r, out[start:]); err != nil {
				return nil, err
			}
		} else if !last {
			// 长度为 0 且不是最后一片：合法但无意义，继续读下一片。
			continue
		}
		if last {
			return out, nil
		}
	}
}

// parseReply 校验 RPC 应答头并返回指向结果区的解码器。
func parseReply(msg []byte, xid uint32) (*xdrDecoder, error) {
	d := newXDRDecoder(msg)

	gotXID := d.uint32()
	gotType := d.uint32()
	if err := d.error(); err != nil {
		return nil, fmt.Errorf("%w: 应答过短: %v", ErrProtocol, err)
	}
	if gotType != msgTypeReply {
		return nil, fmt.Errorf("%w: 消息类型 %d 不是 MSG_REPLY", ErrProtocol, gotType)
	}
	if gotXID != xid {
		return nil, fmt.Errorf("%w: xid 不匹配（期望 %d，收到 %d）", ErrProtocol, xid, gotXID)
	}

	switch stat := d.uint32(); stat {
	case replyStatAccepted:
		// 继续解析下面的校验器和接受状态。
	case replyStatDenied:
		switch reason := d.uint32(); reason {
		case rejectStatRPCMismatch:
			lo, hi := d.uint32(), d.uint32()
			return nil, fmt.Errorf("%w: 服务端支持 RPC 版本 %d..%d", ErrRPCMismatch, lo, hi)
		case rejectStatAuthError:
			return nil, fmt.Errorf("%w: 服务端拒绝认证", ErrAuthError)
		default:
			return nil, fmt.Errorf("%w: 未知的 reject_stat %d", ErrProtocol, reason)
		}
	default:
		return nil, fmt.Errorf("%w: 未知的 reply_stat %d", ErrProtocol, stat)
	}

	// 应答的校验器：flavor + 变长内容，直接跳过。
	d.uint32() // flavor
	d.skipOpaque()

	switch as := d.uint32(); as {
	case acceptStatSuccess:
		// 正常，结果区从当前位置开始。
	case acceptStatProgUnavail:
		return nil, fmt.Errorf("%w: 服务端未注册该程序", ErrProgramUnavailable)
	case acceptStatProgMismatch:
		lo, hi := d.uint32(), d.uint32()
		return nil, fmt.Errorf("%w: 服务端支持版本 %d..%d", ErrProgramMismatch, lo, hi)
	case acceptStatProcUnavail:
		return nil, fmt.Errorf("%w: 服务端不支持该过程", ErrProcedureUnavailable)
	case acceptStatGarbageArgs:
		return nil, fmt.Errorf("%w: 服务端无法解析参数", ErrGarbageArgs)
	case acceptStatSystemErr:
		return nil, fmt.Errorf("%w", ErrSystemError)
	default:
		return nil, fmt.Errorf("%w: 未知的 accept_stat %d", ErrProtocol, as)
	}

	if err := d.error(); err != nil {
		return nil, fmt.Errorf("%w: 应答头解析失败: %v", ErrProtocol, err)
	}
	return d, nil
}

// wrapNetError 给底层网络错误附上语义，便于 errors.Is 判断。
func wrapNetError(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", ErrConnClosed, err)
	}
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrTimeout) ||
		errors.Is(err, ErrTransport) || errors.Is(err, ErrConnClosed) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrTransport, err)
}

// deadlineFor 把超时转换为绝对截止时间；timeout <= 0 表示不设置。
func deadlineFor(timeout time.Duration) (time.Time, bool) {
	if timeout <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(timeout), true
}
