package vxi11

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// 本文件实现 interrupt channel（规范 B.6.12 / B.6.13 / B.6.17 / C.2）：
//
//	1. 客户端在本机起一个 DEVICE_INTR（程序号 0x0607B1，版本 1）的 RPC 服务端；
//	2. 通过 core channel 的 create_intr_chan 把监听地址告诉设备；
//	3. 设备发生 SRQ（服务请求）时，向该地址发起 TCP 连接并调用
//	   device_intr_srq（proc 30），把 device_enable_srq 时登记的 handle 带回来；
//	4. destroy_intr_chan 让设备关闭它那一侧的通道。
//
// 与 core/abort channel 相反，interrupt channel 上客户端是 RPC 服务端、
// 设备是 RPC 客户端（规范 Figure B.4）。

// intrServer 是客户端侧的 DEVICE_INTR RPC 服务端。
type intrServer struct {
	ln      net.Listener
	onSRQ   func(handle []byte) // 回调由 Client.OnSRQ 设置
	done    chan struct{}
	closeMu sync.Once
	wg      sync.WaitGroup
}

// newIntrServer 在随机端口上监听 TCP4（interrupt channel 规范只定义了 IPv4）。
func newIntrServer(onSRQ func(handle []byte)) (*intrServer, error) {
	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("vxi11: 启动 interrupt channel 监听失败: %w", wrapNetError(err))
	}
	s := &intrServer{ln: ln, onSRQ: onSRQ, done: make(chan struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// addr 返回监听地址。
func (s *intrServer) addr() *net.TCPAddr { return s.ln.Addr().(*net.TCPAddr) }

// close 停止监听并等待已建立的连接处理完。
func (s *intrServer) close() {
	s.closeMu.Do(func() {
		close(s.done)
		s.ln.Close()
	})
	s.wg.Wait()
}

func (s *intrServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				// 暂时性错误，稍后重试。
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

// serve 处理设备侧发起的连接：循环读取 device_intr_srq 调用并回送 void 应答。
// 设备可能在一次连接上发送多个 SRQ，也可能每次 SRQ 都新建连接（规范未限定）。
func (s *intrServer) serve(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	for {
		msg, err := readRecord(conn, maxRecordBytes)
		if err != nil {
			return
		}
		reply, handle, ok := s.handleCall(msg)
		if reply != nil {
			conn.SetWriteDeadline(time.Now().Add(DefaultTimeout))
			if err := writeRecord(conn, reply, defaultMaxFragment); err != nil {
				return
			}
		}
		if ok && s.onSRQ != nil {
			s.onSRQ(handle)
		}
	}
}

// handleCall 解析一条 RPC 调用，返回应答报文；ok 为 true 时 handle 有效。
func (s *intrServer) handleCall(msg []byte) (reply, handle []byte, ok bool) {
	d := newXDRDecoder(msg)
	xid := d.uint32()
	mtype := d.uint32()
	rpcvers := d.uint32()
	prog := d.uint32()
	vers := d.uint32()
	proc := d.uint32()
	d.uint32() // cred flavor
	d.skipOpaque()
	d.uint32() // verf flavor
	d.skipOpaque()
	if err := d.error(); err != nil || mtype != msgTypeCall {
		return nil, nil, false
	}
	if rpcvers != rpcVersion {
		return intrDeniedReply(xid, rejectStatRPCMismatch), nil, false
	}

	header := func(acceptStat uint32) *xdrEncoder {
		e := &xdrEncoder{}
		e.uint32(xid)
		e.uint32(msgTypeReply)
		e.uint32(replyStatAccepted)
		e.uint32(authNull)
		e.uint32(0) // 空校验器
		e.uint32(acceptStat)
		return e
	}

	switch {
	case prog != deviceIntrProg:
		e := header(acceptStatProgUnavail)
		return e.bytes(), nil, false
	case vers != deviceIntrVers:
		e := header(acceptStatProgMismatch)
		e.uint32(deviceIntrVers) // low
		e.uint32(deviceIntrVers) // high
		return e.bytes(), nil, false
	case proc != procDeviceIntrSrq:
		e := header(acceptStatProcUnavail)
		return e.bytes(), nil, false
	}

	// device_intr_srq 的参数是 Device_SrqParms{ opaque handle<> }。
	h := d.opaque(40)
	if err := d.error(); err != nil {
		e := header(acceptStatGarbageArgs)
		return e.bytes(), nil, false
	}
	// 过程返回 void：结果区为空。
	e := header(acceptStatSuccess)
	handleCopy := append([]byte(nil), h...)
	return e.bytes(), handleCopy, true
}

// intrDeniedReply 构造 MSG_DENIED 应答（仅用于 RPC 版本不匹配）。
func intrDeniedReply(xid, rejectStat uint32) []byte {
	e := &xdrEncoder{}
	e.uint32(xid)
	e.uint32(msgTypeReply)
	e.uint32(replyStatDenied)
	e.uint32(rejectStat)
	if rejectStat == rejectStatRPCMismatch {
		e.uint32(rpcVersion) // low
		e.uint32(rpcVersion) // high
	}
	return e.bytes()
}

// ---------------------------------------------------------------------------
// Client 侧接口
// ---------------------------------------------------------------------------

// OnSRQ 设置在 interrupt channel 上收到 device_intr_srq 时的回调。
// handle 是 EnableSRQ 时登记的内容，回调里拿到的切片归调用者所有。
// 传 nil 取消回调。回调在服务端 goroutine 里执行，应尽快返回。
func (c *Client) OnSRQ(h func(handle []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intr != nil {
		c.intr.onSRQ = h
	}
	// 即使 interrupt channel 尚未建立，也记住回调，CreateIntrChan 时会用上。
	c.srqHandler = h
}

// CreateIntrChan 建立 interrupt channel（规范 B.6.12）：
// 在本机随机端口启动 DEVICE_INTR 服务端，然后通过 create_intr_chan
// 把地址告诉设备。重复调用是幂等的（已建立时直接返回 nil）。
//
// 建立后设备就能通过 device_enable_srq 打开 SRQ 上报，
// 收到 SRQ 时会回调 OnSRQ 设置的函数。
func (c *Client) CreateIntrChan() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return err
	}
	if c.intr != nil {
		return nil
	}

	srv, err := newIntrServer(c.srqHandler)
	if err != nil {
		return err
	}
	// 通告给设备的地址用 core channel 的本机地址——设备必然能路由到它。
	localIP, err := localIPv4(c.core.conn.LocalAddr())
	if err != nil {
		srv.close()
		return err
	}
	parms := deviceRemoteFunc{
		hostAddr:   binary.BigEndian.Uint32(localIP),
		hostPort:   uint32(srv.addr().Port),
		progNum:    deviceIntrProg,
		progVers:   deviceIntrVers,
		progFamily: addrFamilyTCP,
	}
	t := c.timeoutFor(0)
	if err := c.genericCallLocked("create_intr_chan", procCreateIntrChan, parms.marshal(), t); err != nil {
		srv.close()
		return err
	}
	c.intr = srv
	return nil
}

// DestroyIntrChan 让设备关闭 interrupt channel（规范 B.6.13），
// 并停止本机的 DEVICE_INTR 服务端。未建立时返回 nil（幂等）。
func (c *Client) DestroyIntrChan() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intr == nil {
		return nil
	}
	srv := c.intr
	c.intr = nil

	var firstErr error
	if !c.closed && c.core != nil {
		t := c.timeoutFor(0)
		// destroy_intr_chan 的参数是 void。
		if err := c.genericCallLocked("destroy_intr_chan", procDestroyIntrChan, nil, t); err != nil {
			firstErr = err
		}
	}
	srv.close()
	return firstErr
}

// localIPv4 取出本机侧的 IPv4 地址（4 字节，网络字节序）。
// interrupt channel 规范只定义了 IPv4（hostAddr 是 32 位无符号整数）。
func localIPv4(addr net.Addr) ([]byte, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("vxi11: 无法确定本机地址类型 %T", addr)
	}
	ip4 := tcp.IP.To4()
	if ip4 == nil {
		return nil, errors.New("vxi11: core channel 不是 IPv4，无法建立 interrupt channel")
	}
	return ip4, nil
}
