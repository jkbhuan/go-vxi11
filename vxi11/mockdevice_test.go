package vxi11

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件实现一个内存里的"假仪器"，同时扮演 portmapper、DEVICE_CORE 和
// DEVICE_ASYNC 三个程序，用来在没有任何硬件的情况下端到端测试客户端：
// Dial -> Query -> Read/Write -> Lock/Unlock/Clear -> Close 都会真的走一遍
// ONC RPC + XDR + 记录标记。

// mockWrite 记录一次 device_write 的内容及其 END 标志。
type mockWrite struct {
	data []byte
	end  bool
}

type mockConfig struct {
	device      string            // 合法设备名，默认 "inst0"
	responses   map[string]string // SCPI 命令 -> 文本应答
	maxRecvSize uint32            // create_link 通告的最大写入长度
	abortPort   bool              // 是否通告 abort channel（用自身端口）
	readDelay   time.Duration     // device_read 的额外延迟（用于测试超时）
	failRead    bool              // device_read 直接返回错误码 17
	lockBusy    bool              // 初始状态：锁被别的 link 占用
	stb         byte              // device_readstb 返回的状态字节
	noPortmap   bool              // GETPORT 返回 0，模拟设备未注册
	// zeroReasonReads > 0 时，前 N 次 device_read 返回数据但 reason 全 0
	// （规范 OBSERVATION B.6.9 描述的情形），用来验证客户端会继续读。
	zeroReasonReads int
	// intrChan 为 true 时支持 create_intr_chan/destroy_intr_chan：
	// 收到 create_intr_chan 后回连客户端的 DEVICE_INTR 服务端；
	// device_enable_srq(enable=true) 时会真的发一个 device_intr_srq。
	intrChan bool
	// docmdSupported 为 true 时 device_docmd 返回 docmdResp 作为 data_out；
	// 为 false 时返回错误码 8（operation not supported）。
	docmdSupported bool
	docmdResp      []byte
}

type mockDevice struct {
	cfg  mockConfig
	ln   net.Listener
	port int

	mu          sync.Mutex
	nextLID     int32
	lockOwner   int32 // 0 表示未加锁，-1 表示被其它 link 持有
	clientIDs   []int32
	writes      []mockWrite
	commands    []string
	reads       int
	clears      int
	destroys    int
	aborts      int
	triggers    int
	lockDenied  int
	tooBig      int // 超过 maxRecvSize 的 device_write 次数（真机会拒绝）
	intrCreates int
	intrChanUp  bool     // 设备侧 interrupt channel 是否已建立
	intrConn    net.Conn // 设备侧到客户端 DEVICE_INTR 服务端的连接
	docmds      int
}

// newMock 在 127.0.0.1 上启动一个假仪器，返回的 mockDevice 需要 Close。
func newMock(cfg mockConfig) (*mockDevice, error) {
	if cfg.device == "" {
		cfg.device = "inst0"
	}
	if cfg.maxRecvSize == 0 {
		cfg.maxRecvSize = 4096
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &mockDevice{
		cfg:  cfg,
		ln:   ln,
		port: ln.Addr().(*net.TCPAddr).Port,
	}
	if cfg.lockBusy {
		m.lockOwner = -1
	}
	go m.acceptLoop()
	return m, nil
}

// Close 关闭假仪器的监听。
func (m *mockDevice) Close() { m.ln.Close() }

func startMock(t testing.TB, cfg mockConfig) *mockDevice {
	t.Helper()
	m, err := newMock(cfg)
	if err != nil {
		t.Fatalf("启动假仪器失败: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func (m *mockDevice) acceptLoop() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.serve(c)
	}
}

// reply 发送一个 MSG_ACCEPTED / SUCCESS 的应答，body 是过程结果。
func (m *mockDevice) reply(c net.Conn, xid uint32, body []byte) {
	e := &xdrEncoder{}
	e.uint32(xid)
	e.uint32(msgTypeReply)
	e.uint32(replyStatAccepted)
	e.uint32(authNull)
	e.uint32(0) // 校验器为空
	e.uint32(acceptStatSuccess)
	if err := writeRecord(c, append(e.bytes(), body...), 1<<20); err != nil {
		return
	}
}

func (m *mockDevice) replyStatus(c net.Conn, xid, acceptStat uint32) {
	e := &xdrEncoder{}
	e.uint32(xid)
	e.uint32(msgTypeReply)
	e.uint32(replyStatAccepted)
	e.uint32(authNull)
	e.uint32(0)
	e.uint32(acceptStat)
	_ = writeRecord(c, e.bytes(), 1<<20)
}

// errBody 构造 Device_Error。
func errBody(code int32) []byte {
	e := &xdrEncoder{}
	e.int32(code)
	return e.bytes()
}

func (m *mockDevice) serve(c net.Conn) {
	defer c.Close()

	var (
		lid     int32
		pending []byte // 设备"待发"数据（相当于仪器的输出缓冲）
		accum   []byte // 分片写入的累积缓冲
	)

	for {
		msg, err := readRecord(c, 8<<20)
		if err != nil {
			return
		}
		d := newXDRDecoder(msg)
		xid := d.uint32()
		d.uint32() // mtype
		d.uint32() // rpcvers
		prog := d.uint32()
		vers := d.uint32()
		proc := d.uint32()
		d.uint32() // cred flavor
		d.skipOpaque()
		d.uint32() // verf flavor
		d.skipOpaque()
		if err := d.error(); err != nil {
			return
		}

		if prog == portmapperProg {
			if vers != portmapperVers {
				m.replyStatus(c, xid, acceptStatProgMismatch)
				continue
			}
			switch proc {
			case pmProcGetPort:
				d.uint32() // prog
				d.uint32() // vers
				d.uint32() // proto
				d.uint32() // port
				e := &xdrEncoder{}
				if m.cfg.noPortmap {
					e.uint32(0)
				} else {
					e.uint32(uint32(m.port))
				}
				m.reply(c, xid, e.bytes())
			case pmProcNull:
				m.reply(c, xid, nil)
			default:
				m.replyStatus(c, xid, acceptStatProcUnavail)
			}
			continue
		}

		if prog == deviceAsyncProg {
			if proc == procDeviceAbort {
				d.int32() // lid
				m.mu.Lock()
				m.aborts++
				m.mu.Unlock()
				m.reply(c, xid, errBody(0))
				continue
			}
			m.replyStatus(c, xid, acceptStatProcUnavail)
			continue
		}

		if prog != deviceCoreProg {
			m.replyStatus(c, xid, acceptStatProgUnavail)
			continue
		}
		if vers != deviceCoreVers {
			m.replyStatus(c, xid, acceptStatProgMismatch)
			continue
		}

		switch proc {
		case procCreateLink:
			clientID := d.int32()
			lockDevice := d.boolean()
			lockTimeout := d.uint32()
			device := d.string(1024)
			if err := d.error(); err != nil {
				return
			}
			if device != m.cfg.device {
				e := &xdrEncoder{}
				e.int32(int32(ErrDeviceNotAccessible))
				e.int32(0)
				e.uint16(0)
				e.uint32(0)
				m.reply(c, xid, e.bytes())
				continue
			}

			m.mu.Lock()
			m.nextLID++
			lid = m.nextLID
			m.clientIDs = append(m.clientIDs, clientID)
			busy := m.lockOwner != 0
			m.mu.Unlock()

			if lockDevice && busy {
				if lockTimeout == 0 {
					m.mu.Lock()
					m.lockDenied++
					m.mu.Unlock()
					// 规范要求即使出错也返回完整的 Create_LinkResp。
					e := &xdrEncoder{}
					e.int32(int32(ErrDeviceLocked))
					e.int32(0)
					e.uint16(0)
					e.uint32(0)
					m.reply(c, xid, e.bytes())
					continue
				}
				time.Sleep(time.Duration(lockTimeout) * time.Millisecond)
			}
			if lockDevice {
				m.mu.Lock()
				m.lockOwner = lid
				m.mu.Unlock()
			}

			abortPort := uint16(0)
			if m.cfg.abortPort {
				abortPort = uint16(m.port)
			}
			e := &xdrEncoder{}
			e.int32(0)
			e.int32(lid)
			e.uint16(abortPort)
			e.uint32(m.cfg.maxRecvSize)
			m.reply(c, xid, e.bytes())

		case procDeviceWrite:
			d.int32()  // lid
			d.uint32() // io_timeout
			d.uint32() // lock_timeout
			flags := Flags(d.uint32())
			data := d.opaque(maxOpaqueLen)
			if err := d.error(); err != nil {
				return
			}
			// 真机会拒绝超过 maxRecvSize 的单次写入（实测 RIGOL 返回错误码 5），
			// 这里照做，用来验证客户端确实按 maxRecvSize 分片。
			if uint32(len(data)) > m.cfg.maxRecvSize {
				m.mu.Lock()
				m.tooBig++
				m.mu.Unlock()
				e := &xdrEncoder{}
				e.int32(int32(ErrParameter))
				e.uint32(0)
				m.reply(c, xid, e.bytes())
				continue
			}
			m.mu.Lock()
			m.writes = append(m.writes, mockWrite{data: append([]byte(nil), data...), end: flags&FlagEnd != 0})
			m.mu.Unlock()

			accum = append(accum, data...)
			if flags&FlagEnd != 0 {
				cmd := strings.TrimSpace(string(accum))
				accum = nil
				m.mu.Lock()
				m.commands = append(m.commands, cmd)
				m.mu.Unlock()
				pending = append(pending, []byte(m.cfg.responses[cmd])...)
			}
			e := &xdrEncoder{}
			e.int32(0)
			e.uint32(uint32(len(data)))
			m.reply(c, xid, e.bytes())

		case procDeviceRead:
			d.int32() // lid
			requestSize := d.uint32()
			d.uint32() // io_timeout
			d.uint32() // lock_timeout
			flags := Flags(d.uint32())
			termChar := d.char()
			if err := d.error(); err != nil {
				return
			}
			m.mu.Lock()
			m.reads++
			m.mu.Unlock()
			if m.cfg.readDelay > 0 {
				time.Sleep(m.cfg.readDelay)
			}
			if m.cfg.failRead {
				e := &xdrEncoder{}
				e.int32(int32(ErrIOError))
				e.int32(0)
				e.opaque(nil)
				m.reply(c, xid, e.bytes())
				continue
			}

			var (
				data   []byte
				reason ReadReason
			)
			n := int(requestSize)
			termIdx := -1
			if flags&FlagTermChrSet != 0 && termChar != 0 {
				termIdx = bytes.IndexByte(pending, termChar)
			}
			forceZero := false
			if m.cfg.zeroReasonReads > 0 {
				m.mu.Lock()
				forceZero = m.reads <= m.cfg.zeroReasonReads
				m.mu.Unlock()
			}
			switch {
			case len(pending) == 0:
				reason = ReasonEnd
			case forceZero:
				// 故意返回数据但 reason 全 0：客户端必须继续读，否则会截断。
				k := min(n, len(pending))
				data = pending[:k]
				pending = pending[k:]
				reason = 0
			case termIdx >= 0 && termIdx+1 <= n:
				// 命中终止符，且终止符落在本次请求长度之内。
				data = pending[:termIdx+1]
				pending = pending[termIdx+1:]
				reason = ReasonChr
			case len(pending) <= n:
				data = pending
				pending = nil
				reason = ReasonEnd
			default:
				// 缓冲区被读满：只置 REQCNT，不置 END，客户端必须继续读。
				data = pending[:n]
				pending = pending[n:]
				reason = ReasonReqCnt
			}

			e := &xdrEncoder{}
			e.int32(0)
			e.int32(int32(reason))
			e.opaque(data)
			m.reply(c, xid, e.bytes())

		case procDeviceReadStb:
			d.int32()
			d.uint32()
			d.uint32()
			d.uint32()
			e := &xdrEncoder{}
			e.int32(0)
			e.char(m.cfg.stb)
			m.reply(c, xid, e.bytes())

		case procDeviceClear:
			d.int32()
			d.uint32()
			d.uint32()
			d.uint32()
			pending = nil
			m.mu.Lock()
			m.clears++
			m.mu.Unlock()
			m.reply(c, xid, errBody(0))

		case procDeviceTrigger:
			d.int32()
			d.uint32()
			d.uint32()
			d.uint32()
			m.mu.Lock()
			m.triggers++
			m.mu.Unlock()
			m.reply(c, xid, errBody(0))

		case procDeviceRemote, procDeviceLocal:
			d.int32()
			d.uint32()
			d.uint32()
			d.uint32()
			m.reply(c, xid, errBody(0))

		case procDeviceEnableSrq:
			d.int32()
			enable := d.boolean()
			handle := d.opaque(40)
			if err := d.error(); err != nil {
				return
			}
			if enable && len(handle) > 0 {
				m.sendSRQ(handle)
			}
			m.reply(c, xid, errBody(0))

		case procCreateIntrChan:
			hostAddr := d.uint32()
			hostPort := d.uint32()
			progNum := d.uint32()
			progVers := d.uint32()
			progFamily := d.uint32()
			if err := d.error(); err != nil {
				return
			}
			m.mu.Lock()
			already := m.intrChanUp
			m.mu.Unlock()
			switch {
			case !m.cfg.intrChan || progNum != deviceIntrProg || progVers != deviceIntrVers || progFamily != addrFamilyTCP:
				m.reply(c, xid, errBody(int32(ErrOperationNotSupported)))
			case already:
				m.reply(c, xid, errBody(int32(ErrChannelEstablished)))
			default:
				// 回连客户端的 DEVICE_INTR 服务端（设备此时是 RPC 客户端）。
				addr := net.JoinHostPort(
					net.IPv4(byte(hostAddr>>24), byte(hostAddr>>16), byte(hostAddr>>8), byte(hostAddr)).String(),
					strconv.Itoa(int(hostPort)))
				conn, err := net.DialTimeout("tcp4", addr, 2*time.Second)
				if err != nil {
					m.reply(c, xid, errBody(int32(ErrChannelNotEstablished)))
					continue
				}
				m.mu.Lock()
				m.intrConn = conn
				m.intrChanUp = true
				m.intrCreates++
				m.mu.Unlock()
				m.reply(c, xid, errBody(0))
			}

		case procDestroyIntrChan:
			m.mu.Lock()
			up := m.intrChanUp
			if m.intrConn != nil {
				m.intrConn.Close()
				m.intrConn = nil
			}
			m.intrChanUp = false
			m.mu.Unlock()
			if up {
				m.reply(c, xid, errBody(0))
			} else {
				m.reply(c, xid, errBody(int32(ErrChannelNotEstablished)))
			}

		case procDeviceDocmd:
			d.int32()              // lid
			d.uint32()             // flags
			d.uint32()             // io_timeout
			d.uint32()             // lock_timeout
			d.int32()              // cmd
			d.boolean()            // network_order
			d.int32()              // datasize
			d.opaque(maxOpaqueLen) // data_in
			if err := d.error(); err != nil {
				return
			}
			m.mu.Lock()
			m.docmds++
			m.mu.Unlock()
			e := &xdrEncoder{}
			if m.cfg.docmdSupported {
				e.int32(0)
				e.opaque(m.cfg.docmdResp)
			} else {
				e.int32(int32(ErrOperationNotSupported))
				e.opaque(nil)
			}
			m.reply(c, xid, e.bytes())

		case procDeviceLock:
			d.int32()
			flags := Flags(d.uint32())
			lockTimeout := d.uint32()
			m.mu.Lock()
			free := m.lockOwner == 0 || m.lockOwner == lid
			m.mu.Unlock()
			switch {
			case free:
				m.mu.Lock()
				m.lockOwner = lid
				m.mu.Unlock()
				m.reply(c, xid, errBody(0))
			case flags&FlagWaitLock != 0:
				// 模拟"等到锁被释放"：等一段时间后把锁交给该 link。
				time.Sleep(time.Duration(lockTimeout) * time.Millisecond)
				m.mu.Lock()
				m.lockOwner = lid
				m.mu.Unlock()
				m.reply(c, xid, errBody(0))
			default:
				m.mu.Lock()
				m.lockDenied++
				m.mu.Unlock()
				m.reply(c, xid, errBody(int32(ErrDeviceLocked)))
			}

		case procDeviceUnlock:
			d.int32()
			m.mu.Lock()
			held := m.lockOwner == lid
			if held {
				m.lockOwner = 0
			}
			m.mu.Unlock()
			if held {
				m.reply(c, xid, errBody(0))
			} else {
				m.reply(c, xid, errBody(int32(ErrNoLockHeld)))
			}

		case procDestroyLink:
			d.int32()
			m.mu.Lock()
			m.destroys++
			if m.lockOwner == lid {
				m.lockOwner = 0
			}
			m.mu.Unlock()
			m.reply(c, xid, errBody(0))
			return

		default:
			m.replyStatus(c, xid, acceptStatProcUnavail)
		}
	}
}

// sendSRQ 通过 interrupt channel 向客户端发送 device_intr_srq（proc 30）。
// 设备侧在 interrupt channel 上是 RPC 客户端，因此这里要主动构造调用报文。
func (m *mockDevice) sendSRQ(handle []byte) {
	m.mu.Lock()
	conn := m.intrConn
	m.mu.Unlock()
	if conn == nil {
		return
	}
	e := &xdrEncoder{}
	e.uint32(0xABCD) // xid
	e.uint32(msgTypeCall)
	e.uint32(rpcVersion)
	e.uint32(deviceIntrProg)
	e.uint32(deviceIntrVers)
	e.uint32(procDeviceIntrSrq)
	e.uint32(authNull)
	e.uint32(0)
	e.uint32(authNull)
	e.uint32(0)
	e.opaque(handle)
	if err := writeRecord(conn, e.bytes(), 1<<20); err != nil {
		return
	}
	// 读取客户端的 void 应答，保持字节流对齐。
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = readRecord(conn, 1<<20)
}

// snapshot 返回写入记录与统计数据的副本。
func (m *mockDevice) snapshot() (writes []mockWrite, commands []string, stats map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writes = append(writes, m.writes...)
	commands = append(commands, m.commands...)
	stats = map[string]int{
		"reads":       m.reads,
		"clears":      m.clears,
		"destroys":    m.destroys,
		"aborts":      m.aborts,
		"triggers":    m.triggers,
		"lockDenied":  m.lockDenied,
		"tooBig":      m.tooBig,
		"intrCreates": m.intrCreates,
		"docmds":      m.docmds,
	}
	return writes, commands, stats
}
