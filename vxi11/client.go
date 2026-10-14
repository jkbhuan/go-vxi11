// Package vxi11 是一个纯 Go 实现的 VXI-11（TCP/IP Instrument Protocol）客户端，
// 用于通过 LAN 控制支持 VXI-11 的仪器（示波器、信号源、万用表等）。
//
// 它自己实现了 ONC RPC（RFC 5531）在 TCP 上的记录标记、XDR（RFC 4506）编解码
// 以及 portmapper（rpcbind）端口发现，不依赖任何第三方库，也不依赖 cgo。
//
// 典型用法：
//
//	c, err := vxi11.Dial("5.5.5.1", "inst0", 5*time.Second)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer c.Close()
//
//	idn, err := c.Query("*IDN?")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println(idn)
//
// 所有耗时方法都接收（或使用）一个 I/O 超时；Client 可以并发使用，
// 同一条 core channel 上的调用会被串行化。
package vxi11

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultPortmapperPort 是 ONC RPC portmapper（rpcbind）的标准端口。
	DefaultPortmapperPort = 111

	// DefaultDevice 是 LAN 仪器最常用的 VXI-11 设备名。
	// 规范本身没有规定这个字符串，它是 IVI/VISA 生态的约定，实际以仪器为准。
	DefaultDevice = "inst0"

	// DefaultTimeout 是 Dial 以及各方法在超时参数 <= 0 时使用的默认超时。
	DefaultTimeout = 5 * time.Second

	// minWriteChunk 是 device_write 分片的下限（规范保证 maxRecvSize 至少为 1024）。
	minWriteChunk = 1024
	// maxWriteChunk 是 device_write 分片的上限，避免设备通告一个过大的 maxRecvSize。
	maxWriteChunk = 1 << 20

	// defaultReadSize 是单次 device_read 请求的默认长度（可用 WithReadSize 覆盖）。
	//
	// 规范只规定 maxRecvSize 是 device_write 的上限，并没有限制 device_read 的
	// requestSize——设备可以只返回它当前拥有的数据。所以这里不跟随 maxRecvSize，
	// 直接用 64 KiB，把大响应的往返次数降下来。实测 RIGOL DS1104Z 读 1.1 MB
	// 屏幕数据：requestSize=1500（=该机的 maxRecvSize）耗时 72 秒，
	// requestSize=64 KiB 只需 3.4 秒。
	defaultReadSize = 64 << 10
	// maxReadSizeLimit 是单次 device_read 请求长度的上限。
	maxReadSizeLimit = 1 << 20

	// maxRecordBytes 是单条 RPC 消息（写或读）允许的最大字节数。
	maxRecordBytes = 64 << 20
	// DefaultMaxResponseBytes 是一次 Query 允许累积的最大响应字节数。
	DefaultMaxResponseBytes = 64 << 20

	// maxReadIterations 是单次读循环的最大 device_read 次数，防止死循环。
	maxReadIterations = 100000

	// ioTimeoutMarginMax / ioTimeoutMarginMin 决定 socket 截止时间相对设备侧
	// io_timeout 的余量（见 socketTimeout）：让设备有机会先返回错误码 15
	// （I/O timeout），而不是客户端直接掐断连接、把字节流搞乱。
	ioTimeoutMarginMax = time.Second
	ioTimeoutMarginMin = 100 * time.Millisecond
)

// Option 用于定制 Dial 的行为，均为可选。
type Option func(*dialConfig) error

type dialConfig struct {
	portmapperPort int
	corePort       int
	clientID       int32
	lockOnCreate   bool
	createLockTO   time.Duration
	termChar       byte
	readSize       uint32
	writeChunk     int // -1 = 跟随 maxRecvSize，0 = 不分片，>0 = 指定分片大小
	maxResponse    int
	abortChannel   bool
	dialer         *net.Dialer
}

func defaultDialConfig() dialConfig {
	return dialConfig{
		portmapperPort: DefaultPortmapperPort,
		clientID:       rand.Int31(),
		termChar:       '\n',
		writeChunk:     -1,
		maxResponse:    DefaultMaxResponseBytes,
		abortChannel:   true,
		dialer:         &net.Dialer{},
	}
}

// WithPortmapperPort 指定 portmapper 的端口（默认 111）。
// 主要用于测试或非标准环境。
func WithPortmapperPort(port int) Option {
	return func(c *dialConfig) error {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("vxi11: 非法的 portmapper 端口 %d", port)
		}
		c.portmapperPort = port
		return nil
	}
}

// WithCorePort 直接使用给定的 core channel 端口，跳过 portmapper 查询。
//
// 规范建议始终通过 portmapper 发现端口，因此该选项仅用于
// 设备未运行 portmapper 或调试时锁定端口的场景。
func WithCorePort(port int) Option {
	return func(c *dialConfig) error {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("vxi11: 非法的 core channel 端口 %d", port)
		}
		c.corePort = port
		return nil
	}
}

// WithClientID 指定 create_link 上报的 clientId（默认随机）。
// 该值只用于设备端调试追踪。
func WithClientID(id int32) Option {
	return func(c *dialConfig) error {
		c.clientID = id
		return nil
	}
}

// WithLockOnCreate 让 create_link 直接尝试加锁（lockDevice=true），
// 等待锁的超时为 timeout（<=0 表示不等待，被占用则返回错误码 11）。
func WithLockOnCreate(lock bool, timeout time.Duration) Option {
	return func(c *dialConfig) error {
		c.lockOnCreate = lock
		c.createLockTO = timeout
		return nil
	}
}

// WithTermChar 设置 device_read 使用终止符（termchrset）时的终止字符，
// 默认 '\n'。设为 0 表示 Query 也不使用终止符，完全依赖设备返回的 END 指示。
func WithTermChar(ch byte) Option {
	return func(c *dialConfig) error {
		c.termChar = ch
		return nil
	}
}

// WithReadSize 指定单次 device_read 请求的字节数（默认 64 KiB）。
//
// 设备可以只返回它当前拥有的数据，所以请求大一些不会出错，只会减少往返次数；
// 若某台设备对 requestSize 特别敏感，可以用本选项调小。
func WithReadSize(n uint32) Option {
	return func(c *dialConfig) error {
		if n == 0 {
			return errors.New("vxi11: readSize 不能为 0")
		}
		c.readSize = n
		return nil
	}
}

// WithMaxResponseBytes 限制一次 Query/QueryBytes 允许累积的最大响应字节数
// （默认 64 MiB），超出返回 ErrResponseTooLarge。
func WithMaxResponseBytes(n int) Option {
	return func(c *dialConfig) error {
		if n <= 0 {
			return errors.New("vxi11: maxResponseBytes 必须为正数")
		}
		c.maxResponse = n
		return nil
	}
}

// WithAbortChannel 控制是否在 create_link 之后尝试建立 abort channel（默认 true）。
// abort channel 是可选的，建立失败不影响正常收发。
func WithAbortChannel(enable bool) Option {
	return func(c *dialConfig) error {
		c.abortChannel = enable
		return nil
	}
}

// WithDialer 使用自定义的 net.Dialer（可配置 LocalAddr、KeepAlive 等）。
// 其 Timeout 字段会被 Dial 的 timeout 参数覆盖（若为 0）。
func WithDialer(d *net.Dialer) Option {
	return func(c *dialConfig) error {
		if d == nil {
			return errors.New("vxi11: dialer 不能为 nil")
		}
		c.dialer = d
		return nil
	}
}

// WithWriteChunkSize 指定 device_write 的分片大小：
//
//	n > 0：每片最多 n 字节，只有最后一片带 END 标志；
//	n == 0：不分片，整条命令用一次 device_write 发出；
//	不调用本选项（默认）：跟随 create_link 返回的 maxRecvSize。
//
// 规范要求单次 device_write 的数据不超过设备通告的 maxRecvSize，因此默认按
// maxRecvSize 分片；但实测部分仪器（如 RIGOL DS1000Z 系列）会把每个
// device_write 当成一条完整消息解析，此时需要用 0 关闭分片。
func WithWriteChunkSize(n int) Option {
	return func(c *dialConfig) error {
		if n < 0 {
			return fmt.Errorf("vxi11: 非法的 device_write 分片大小 %d", n)
		}
		c.writeChunk = n
		return nil
	}
}

// Client 是一条已经建立好的 VXI-11 link。
//
// 它内部维护 core channel 与可选的 abort channel 两条 TCP 连接；
// 所有方法都是并发安全的（对 core channel 的调用会被串行化），
// 但请注意：方法之间不会自动配对，多个 goroutine 混用 Query 时
// 各自得到的是自己那次 Query 的应答（因为 Query 内部持锁完成 write+read）。
type Client struct {
	host   string
	device string
	port   int

	lid         int32
	abortPort   uint16
	maxRecvSize uint32

	core  *rpcConn
	abort *rpcConn

	// intr 是客户端侧的 DEVICE_INTR 服务端（interrupt channel），未建立时为 nil。
	intr       *intrServer
	srqHandler func(handle []byte)

	timeout     time.Duration
	termChar    byte
	readSize    uint32
	writeChunk  int
	maxResponse int

	// mu 串行化 core channel 上的一切操作（包括 Query 的 write+read 组合）。
	mu sync.Mutex
	// abortMu 单独保护 abort channel：Abort 不应被正在进行的长时间读写阻塞。
	abortMu sync.Mutex

	closed bool
	// destroyed 记录 destroy_link 是否已成功发出，供 Close 幂等判断。
	destroyed bool
}

// Dial 连接一台 VXI-11 设备：先通过 portmapper 查询 DEVICE_CORE 的端口，
// 建立 core channel，再发 create_link。
//
// host 可以是 IP 或主机名（不要带端口）。device 通常是 "inst0"，
// 传空字符串时使用 DefaultDevice。timeout <= 0 时使用 DefaultTimeout，
// 它同时作为连接超时和 create_link 的 I/O 超时。
//
// 返回的 Client 使用完必须 Close（会发送 destroy_link 并关闭连接）。
func Dial(host, device string, timeout time.Duration, opts ...Option) (*Client, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("vxi11: host 不能为空")
	}
	cfg := defaultDialConfig()
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if device == "" {
		device = DefaultDevice
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	dialer := *cfg.dialer
	if dialer.Timeout == 0 {
		dialer.Timeout = timeout
	}

	port := cfg.corePort
	if port == 0 {
		p, err := discoverPort(&dialer, host, cfg.portmapperPort, timeout)
		if err != nil {
			return nil, err
		}
		port = p
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	coreConn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("vxi11: 连接 core channel %s 失败: %w", addr, wrapNetError(err))
	}
	core := newRPCConn(coreConn, deviceCoreProg, deviceCoreVers)

	parms := createLinkParms{
		clientID:    cfg.clientID,
		lockDevice:  cfg.lockOnCreate,
		lockTimeout: durationToMillis(cfg.createLockTO),
		device:      device,
	}
	d, err := core.call(procCreateLink, parms.marshal(), timeout)
	if err != nil {
		core.close()
		return nil, fmt.Errorf("vxi11: create_link(%s) 失败: %w", addr, err)
	}
	resp := parseCreateLinkResp(d)
	if err := d.error(); err != nil {
		core.close()
		return nil, fmt.Errorf("vxi11: create_link 应答解析失败: %w", err)
	}
	if err := deviceErr("create_link", resp.err); err != nil {
		core.close()
		return nil, err
	}

	c := &Client{
		host:        host,
		device:      device,
		port:        port,
		lid:         resp.lid,
		abortPort:   resp.abortPort,
		maxRecvSize: resp.maxRecvSize,
		core:        core,
		timeout:     timeout,
		termChar:    cfg.termChar,
		readSize:    cfg.readSize,
		writeChunk:  cfg.writeChunk,
		maxResponse: cfg.maxResponse,
	}

	// abort channel 是可选通道：建立失败只影响 Abort()，不影响常规收发。
	if cfg.abortChannel && resp.abortPort != 0 {
		abortAddr := net.JoinHostPort(host, strconv.Itoa(int(resp.abortPort)))
		if conn, err := dialer.Dial("tcp", abortAddr); err == nil {
			c.abort = newRPCConn(conn, deviceAsyncProg, deviceAsyncVers)
		}
	}
	return c, nil
}

// discoverPort 通过 portmapper 的 PMAPPROC_GETPORT 查询 DEVICE_CORE 的 TCP 端口。
func discoverPort(dialer *net.Dialer, host string, pmPort int, timeout time.Duration) (int, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(pmPort))
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("vxi11: 连接 portmapper %s 失败: %w", addr, wrapNetError(err))
	}
	defer conn.Close()

	pm := newRPCConn(conn, portmapperProg, portmapperVers)
	e := &xdrEncoder{}
	e.uint32(deviceCoreProg)
	e.uint32(deviceCoreVers)
	e.uint32(ipProtoTCP)
	e.uint32(0) // port 参数在 GETPORT 中不使用
	d, err := pm.call(pmProcGetPort, e.bytes(), timeout)
	if err != nil {
		return 0, fmt.Errorf("vxi11: portmapper GETPORT(prog=%#x, vers=%d) 失败: %w",
			deviceCoreProg, deviceCoreVers, err)
	}
	port := int(d.uint32())
	if err := d.error(); err != nil {
		return 0, fmt.Errorf("vxi11: portmapper 应答解析失败: %w", err)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%w: %s 未注册 DEVICE_CORE (prog=%#x, vers=%d, 返回端口 %d)",
			ErrPortmapper, host, deviceCoreProg, deviceCoreVers, port)
	}
	return port, nil
}

// Host 返回连接的主机名/IP。
func (c *Client) Host() string { return c.host }

// Device 返回 create_link 时使用的设备名。
func (c *Client) Device() string { return c.device }

// Port 返回 DEVICE_CORE 所在的 TCP 端口。
func (c *Client) Port() int { return c.port }

// LinkID 返回 create_link 分配到的 Link ID（lid）。
func (c *Client) LinkID() int32 { return c.lid }

// AbortPort 返回设备通告的 abort channel 端口，0 表示设备不提供。
func (c *Client) AbortPort() uint16 { return c.abortPort }

// MaxRecvSize 返回设备通告的单次 device_write 最大字节数。
func (c *Client) MaxRecvSize() uint32 { return c.maxRecvSize }

// Timeout 返回客户端默认的 I/O 超时。
func (c *Client) Timeout() time.Duration { return c.timeout }

// String 返回便于日志输出的描述。
func (c *Client) String() string {
	return fmt.Sprintf("vxi11.Client{host=%s device=%s lid=%d maxRecvSize=%d}",
		c.host, c.device, c.lid, c.maxRecvSize)
}

// checkOpen 必须在持有 c.mu 时调用。
func (c *Client) checkOpen() error {
	if c.closed || c.core == nil {
		return ErrClosed
	}
	return nil
}

// timeoutFor 把方法级超时归一化：<=0 时使用客户端默认超时。
func (c *Client) timeoutFor(ioTimeout time.Duration) time.Duration {
	if ioTimeout <= 0 {
		return c.timeout
	}
	return ioTimeout
}

// socketTimeout 返回 socket 层的超时时间：在设备侧 io_timeout 之上再留一点余量，
// 让设备优先用自己的超时逻辑返回错误码 15。余量取 d/2 与 100ms 中的较大者，
// 上限 1 秒，这样短超时也能被如实反映出来。
func socketTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	margin := d/2 + ioTimeoutMarginMin
	if margin > ioTimeoutMarginMax {
		margin = ioTimeoutMarginMax
	}
	return d + margin
}

// writeChunkSize 返回 device_write 的分片大小；0 表示不分片（一次写完）。
func (c *Client) writeChunkSize() int {
	if c.writeChunk >= 0 {
		return c.writeChunk
	}
	n := int(c.maxRecvSize)
	if n < minWriteChunk {
		return minWriteChunk
	}
	if n > maxWriteChunk {
		return maxWriteChunk
	}
	return n
}

// readRequestSize 返回单次 device_read 请求的字节数（默认 defaultReadSize）。
func (c *Client) readRequestSize() uint32 {
	if c.readSize != 0 {
		if c.readSize > maxReadSizeLimit {
			return maxReadSizeLimit
		}
		return c.readSize
	}
	return defaultReadSize
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Write 把 data 写入设备，返回设备实际接收的字节数。
//
// 若 data 长度超过设备通告的 maxRecvSize，会自动分片发送；
// 只有最后一片带 END 标志（规范 Section C.1：end 表示"本条消息的最后一个字节"）。
// ioTimeout <= 0 时使用客户端默认超时。
func (c *Client) Write(data []byte, ioTimeout time.Duration) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeLocked(data, ioTimeout, true)
}

// WriteString 是 Write 的便利封装：写入 SCPI 命令，若未以 '\n' 结尾会自动补上。
func (c *Client) WriteString(cmd string) error {
	if !strings.HasSuffix(cmd, "\n") {
		cmd += "\n"
	}
	_, err := c.Write([]byte(cmd), 0)
	return err
}

// writeLocked 在持有 c.mu 的前提下执行 device_write（必要时分片）。
func (c *Client) writeLocked(data []byte, ioTimeout time.Duration, end bool) (int, error) {
	if err := c.checkOpen(); err != nil {
		return 0, err
	}
	t := c.timeoutFor(ioTimeout)
	chunk := c.writeChunkSize()

	written := 0
	rest := data
	for {
		n := len(rest)
		if chunk > 0 && n > chunk {
			n = chunk
		}
		part := rest[:n]
		rest = rest[n:]

		var flags Flags
		if end && len(rest) == 0 {
			flags |= FlagEnd
		}
		parms := deviceWriteParms{
			lid:         c.lid,
			ioTimeout:   durationToMillis(t),
			lockTimeout: 0,
			flags:       flags,
			data:        part,
		}
		d, err := c.core.call(procDeviceWrite, parms.marshal(), socketTimeout(t))
		if err != nil {
			return written, fmt.Errorf("vxi11: device_write 失败: %w", err)
		}
		resp := parseDeviceWriteResp(d)
		if err := d.error(); err != nil {
			return written, fmt.Errorf("vxi11: device_write 应答解析失败: %w", err)
		}
		if err := deviceErr("device_write", resp.err); err != nil {
			return written, err
		}
		written += int(resp.size)
		if int(resp.size) != len(part) {
			return written, fmt.Errorf("%w: device_write 只写入 %d/%d 字节",
				ErrShortWrite, resp.size, len(part))
		}
		if len(rest) == 0 {
			return written, nil
		}
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Read 执行一次 device_read，最多读取 maxLen 字节
// （maxLen <= 0 表示使用默认请求长度，见 defaultReadSize / WithReadSize）。
//
// 它不设置终止符（termchrset），因此对二进制数据是安全的；
// 一次 Read 只对应一次 RPC，返回的数据量可能小于请求量——需要"读到结束为止"
// 请用 ReadAll，或者直接用 Query/QueryBytes。
func (c *Client) Read(maxLen int, ioTimeout time.Duration) ([]byte, error) {
	data, _, err := c.ReadWithReason(maxLen, ioTimeout)
	return data, err
}

// ReadWithReason 与 Read 相同，但额外返回设备给出的 reason 位
// （ReasonReqCnt / ReasonChr / ReasonEnd，见规范 Table B.7）。
func (c *Client) ReadWithReason(maxLen int, ioTimeout time.Duration) ([]byte, ReadReason, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := c.readRequestSize()
	if maxLen > 0 && uint32(maxLen) < size {
		size = uint32(maxLen)
	}
	return c.readOnceLocked(size, ioTimeout, false)
}

// ReadAll 反复调用 device_read 直到设备指示结束（reason 含 END 或 CHR）、
// 读到空数据或达到 maxLen/客户端上限，把多次读取的数据拼接后返回。
//
// 这是处理长响应的正确方式：规范 OBSERVATION B.6.9 指出，一次 device_read
// 可能因为内部缓冲满而返回且 reason 全为 0，此时必须继续读，
// 否则长回复（如 :DISPLAY:DATA? 的二进制块）会被截断。
//
// 不设置终止符，二进制安全。出错时返回的 data 可能包含已成功读取的部分数据。
func (c *Client) ReadAll(maxLen int, ioTimeout time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readAllLocked(maxLen, ioTimeout, false)
}

// readOnceLocked 执行一次 device_read（必须在持有 c.mu 时调用）。
func (c *Client) readOnceLocked(size uint32, ioTimeout time.Duration, useTerm bool) ([]byte, ReadReason, error) {
	if err := c.checkOpen(); err != nil {
		return nil, 0, err
	}
	if size == 0 {
		size = defaultReadSize
	}
	t := c.timeoutFor(ioTimeout)

	var flags Flags
	if useTerm && c.termChar != 0 {
		flags |= FlagTermChrSet
	}
	parms := deviceReadParms{
		lid:         c.lid,
		requestSize: size,
		ioTimeout:   durationToMillis(t),
		lockTimeout: 0,
		flags:       flags,
		termChar:    c.termChar,
	}
	d, err := c.core.call(procDeviceRead, parms.marshal(), socketTimeout(t))
	if err != nil {
		return nil, 0, fmt.Errorf("vxi11: device_read 失败: %w", err)
	}
	resp := parseDeviceReadResp(d)
	if err := d.error(); err != nil {
		return nil, 0, fmt.Errorf("vxi11: device_read 应答解析失败: %w", err)
	}
	if err := deviceErr("device_read", resp.err); err != nil {
		return nil, resp.reason, err
	}
	// 复制一份：resp.data 指向本次 RPC 的应答缓冲区。
	out := make([]byte, len(resp.data))
	copy(out, resp.data)
	return out, resp.reason, nil
}

// readAllLocked 循环读取直到结束条件满足（必须在持有 c.mu 时调用）。
func (c *Client) readAllLocked(maxLen int, ioTimeout time.Duration, useTerm bool) ([]byte, error) {
	limit := c.maxResponse
	if maxLen > 0 && maxLen < limit {
		limit = maxLen
	}
	size := c.readRequestSize()

	var out []byte
	for i := 0; i < maxReadIterations; i++ {
		need := size
		if remain := limit - len(out); remain <= 0 {
			return out, nil
		} else if uint32(remain) < need {
			need = uint32(remain)
		}
		chunk, reason, err := c.readOnceLocked(need, ioTimeout, useTerm)
		if err != nil {
			return out, err
		}
		out = append(out, chunk...)

		switch {
		case len(chunk) == 0:
			// 设备没有更多数据了（即使 reason 全 0，也不能无限读下去）。
			return out, nil
		case reason.Has(ReasonEnd) || reason.Has(ReasonChr):
			// END：设备指示消息结束；CHR：命中终止符。两种情况都可以收工。
			return out, nil
		case len(out) >= limit:
			return out, fmt.Errorf("%w: 已累计读取 %d 字节", ErrResponseTooLarge, len(out))
		}
		// 只剩 ReasonReqCnt 或 reason==0：缓冲区读满/被内部限制打断，继续读。
	}
	return out, fmt.Errorf("%w: 连续 %d 次 device_read 仍未读到结束标志",
		ErrProtocol, maxReadIterations)
}

// ---------------------------------------------------------------------------
// 查询（write + read 组合）
// ---------------------------------------------------------------------------

// Query 执行一次标准的 SCPI 查询：写入 cmd（自动补 '\n'），
// 然后循环读取直到设备指示结束，返回去掉结尾 CR/LF 的文本。
//
// 读取时启用终止符 '\n'（termchrset），这样即使设备不发 END 也能正常收尾；
// 二进制数据请用 QueryBytes。
func (c *Client) Query(cmd string) (string, error) {
	data, err := c.queryBytes(cmd, 0, true)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// QueryWithTimeout 与 Query 相同，但可以指定 I/O 超时（<=0 表示默认超时）。
func (c *Client) QueryWithTimeout(cmd string, ioTimeout time.Duration) (string, error) {
	data, err := c.queryBytes(cmd, ioTimeout, true)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// QueryBytes 与 Query 相同，但返回原始字节，且不设置终止符
// （二进制安全，适用于 :DISPLAY:DATA? 这类 IEEE 488.2 定长块应答）。
//
// cmd 会被原样发送（不自动补 '\n'），需要时请自行拼上。
func (c *Client) QueryBytes(cmd string, ioTimeout time.Duration) ([]byte, error) {
	return c.queryBytes(cmd, ioTimeout, false)
}

// queryBytes 是 Query/QueryBytes 的共同实现。useTerm 为 true 时启用终止符。
func (c *Client) queryBytes(cmd string, ioTimeout time.Duration, useTerm bool) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	payload := []byte(cmd)
	if useTerm && !strings.HasSuffix(cmd, "\n") {
		payload = append(payload, '\n')
	}
	if _, err := c.writeLocked(payload, ioTimeout, true); err != nil {
		return nil, err
	}
	return c.readAllLocked(0, ioTimeout, useTerm)
}

// ---------------------------------------------------------------------------
// 通用操作
// ---------------------------------------------------------------------------

// doGeneric 执行一个 Device_GenericParms 形式的过程并检查响应里的错误码。
func (c *Client) doGeneric(op string, proc uint32, ioTimeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return err
	}
	t := c.timeoutFor(ioTimeout)
	parms := deviceGenericParms{
		lid:         c.lid,
		flags:       0,
		lockTimeout: 0,
		ioTimeout:   durationToMillis(t),
	}
	return c.genericCallLocked(op, proc, parms.marshal(), t)
}

// genericCallLocked 发送一个无返回数据（只有 Device_Error）的过程。
// ioTimeout 是设备侧 I/O 超时，socket 截止时间会在其基础上多留 ioTimeoutMargin。
func (c *Client) genericCallLocked(op string, proc uint32, parms []byte, ioTimeout time.Duration) error {
	if op == "" {
		op = procName(proc)
	}
	d, err := c.core.call(proc, parms, socketTimeout(ioTimeout))
	if err != nil {
		return fmt.Errorf("vxi11: %s 失败: %w", op, err)
	}
	code := d.int32()
	if err := d.error(); err != nil {
		return fmt.Errorf("vxi11: %s 应答解析失败: %w", op, err)
	}
	return deviceErr(op, code)
}

// Clear 发送 device_clear：清空设备的输入/输出缓冲并复位待处理的操作。
func (c *Client) Clear() error { return c.doGeneric("device_clear", procDeviceClear, 0) }

// Trigger 发送 device_trigger：触发一次设备操作（等价于 SCPI 的 *TRG）。
func (c *Client) Trigger() error { return c.doGeneric("device_trigger", procDeviceTrigger, 0) }

// Remote 发送 device_remote：让设备进入远程控制状态。
func (c *Client) Remote() error { return c.doGeneric("device_remote", procDeviceRemote, 0) }

// Local 发送 device_local：让设备回到本地控制状态。
func (c *Client) Local() error { return c.doGeneric("device_local", procDeviceLocal, 0) }

// ReadSTB 发送 device_readstb，读取设备的状态字节（Status Byte，等价于 SCPI 的 *STB?）。
func (c *Client) ReadSTB() (byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return 0, err
	}
	t := c.timeoutFor(0)
	parms := deviceGenericParms{
		lid:         c.lid,
		flags:       0,
		lockTimeout: 0,
		ioTimeout:   durationToMillis(t),
	}
	d, err := c.core.call(procDeviceReadStb, parms.marshal(), socketTimeout(t))
	if err != nil {
		return 0, fmt.Errorf("vxi11: device_readstb 失败: %w", err)
	}
	resp := parseDeviceReadStbResp(d)
	if err := d.error(); err != nil {
		return 0, fmt.Errorf("vxi11: device_readstb 应答解析失败: %w", err)
	}
	if err := deviceErr("device_readstb", resp.err); err != nil {
		return 0, err
	}
	return resp.stb, nil
}

// EnableSRQ 发送 device_enable_srq，启用/禁用 SRQ 中断上报。
// handle 是中断通道句柄（规范限制最大 40 字节），设备触发 SRQ 时会
// 原样随 device_intr_srq 带回，用于区分是哪个 link 在请求服务。
//
// 注意：SRQ 上报需要先用 CreateIntrChan 建立 interrupt channel；
// 未建立时本过程通常仍返回成功（设备只记录状态），但不会收到任何 SRQ 通知。
func (c *Client) EnableSRQ(enable bool, handle []byte) error {
	if len(handle) > 40 {
		return fmt.Errorf("vxi11: device_enable_srq 的 handle 最长 40 字节，实际 %d", len(handle))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return err
	}
	t := c.timeoutFor(0)
	parms := deviceEnableSrqParms{lid: c.lid, enable: enable, handle: handle}
	return c.genericCallLocked("device_enable_srq", procDeviceEnableSrq, parms.marshal(), t)
}

// Docmd 发送 device_docmd（proc 22），执行设备相关命令（规范 B.6.15）。
//
// cmd 的取值由设备厂商定义（规范把所有值都保留给相关文档），
// 设备不支持某个 cmd 时返回错误码 8（ErrOperationNotSupported）。
// networkOrder 表示本机字节序是否为网络序（大端）：x86/ARM 上运行
// 的常见系统都是小端，应传 false；设备据此决定是否对 data 做字节交换。
// datasize 是 data 中单个数据元素的字节数（1 表示不透明字节流，不做交换）。
// 返回值是设备的 data_out。ioTimeout <= 0 时使用客户端默认超时。
//
// 绝大多数 LAN 示波器场景用不到本过程（典型用途是 GPIB 网关控制）。
func (c *Client) Docmd(cmd int32, networkOrder bool, datasize int32, data []byte, ioTimeout time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	t := c.timeoutFor(ioTimeout)
	parms := deviceDocmdParms{
		lid:          c.lid,
		flags:        0,
		ioTimeout:    durationToMillis(t),
		lockTimeout:  0,
		cmd:          cmd,
		networkOrder: networkOrder,
		datasize:     datasize,
		data:         data,
	}
	d, err := c.core.call(procDeviceDocmd, parms.marshal(), socketTimeout(t))
	if err != nil {
		return nil, fmt.Errorf("vxi11: device_docmd 失败: %w", err)
	}
	resp := parseDeviceDocmdResp(d)
	if err := d.error(); err != nil {
		return nil, fmt.Errorf("vxi11: device_docmd 应答解析失败: %w", err)
	}
	if err := deviceErr("device_docmd", resp.err); err != nil {
		return nil, err
	}
	out := make([]byte, len(resp.data))
	copy(out, resp.data)
	return out, nil
}

// ---------------------------------------------------------------------------
// 锁
// ---------------------------------------------------------------------------

// Lock 发送 device_lock 获取设备锁。
//
// timeout > 0 时置 waitlock 位并等待最多 timeout（毫秒精度）；timeout <= 0 表示
// 锁被占用时立刻返回错误码 11（ErrDeviceLocked）。
func (c *Client) Lock(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return err
	}
	t := c.timeoutFor(0)
	var flags Flags
	var lockTO uint32
	if timeout > 0 {
		flags |= FlagWaitLock
		lockTO = durationToMillis(timeout)
	}
	// 等锁时间可能远长于默认 I/O 超时，socket 截止时间要跟着放宽。
	if timeout > 0 {
		t += timeout
	}
	parms := deviceLockParms{lid: c.lid, flags: flags, lockTimeout: lockTO}
	return c.genericCallLocked("device_lock", procDeviceLock, parms.marshal(), t)
}

// Unlock 发送 device_unlock 释放设备锁（未持锁时设备返回错误码 12）。
func (c *Client) Unlock() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(); err != nil {
		return err
	}
	t := c.timeoutFor(0)
	parms := deviceLink{lid: c.lid}
	return c.genericCallLocked("device_unlock", procDeviceUnlock, parms.marshal(), t)
}

// ---------------------------------------------------------------------------
// abort channel
// ---------------------------------------------------------------------------

// Abort 通过 abort channel 向设备发送 device_abort，中断当前 link 上正在进行的操作。
//
// 它使用独立的连接和独立的锁，因此可以（也应该）在另一个 goroutine
// 正在 Read/Query 时被调用。设备没有提供 abortPort 时返回 ErrNoAbortChannel。
func (c *Client) Abort() error {
	c.abortMu.Lock()
	defer c.abortMu.Unlock()
	if c.abort == nil {
		return fmt.Errorf("%w: 设备未提供 abortPort", ErrNoAbortChannel)
	}
	parms := deviceLink{lid: c.lid}
	d, err := c.abort.call(procDeviceAbort, parms.marshal(), socketTimeout(c.timeout))
	if err != nil {
		return fmt.Errorf("vxi11: device_abort 失败: %w", err)
	}
	code := d.int32()
	if err := d.error(); err != nil {
		return fmt.Errorf("vxi11: device_abort 应答解析失败: %w", err)
	}
	return deviceErr("device_abort", code)
}

// ---------------------------------------------------------------------------
// 关闭
// ---------------------------------------------------------------------------

// Close 发送 destroy_link 并关闭 core/abort 两条连接。
//
// 它是幂等的：重复调用只会返回 nil。即使 destroy_link 失败，
// 连接也一定会被关闭；此时返回 destroy_link 的错误供调用方记录。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	core := c.core
	lid := c.lid
	intr := c.intr
	c.intr = nil
	c.mu.Unlock()

	var firstErr error
	if core != nil && !c.destroyed {
		// 先关 interrupt channel（设备侧释放通道），再 destroy_link。
		// 拆除失败不阻断关闭流程。
		if intr != nil {
			_, _ = core.call(procDestroyIntrChan, nil, DefaultTimeout)
			intr.close()
		}
		d, err := core.call(procDestroyLink, deviceLink{lid: lid}.marshal(), DefaultTimeout)
		switch {
		case err != nil:
			firstErr = fmt.Errorf("vxi11: destroy_link 失败: %w", err)
		default:
			code := d.int32()
			if perr := d.error(); perr != nil {
				firstErr = fmt.Errorf("vxi11: destroy_link 应答解析失败: %w", perr)
			} else if e := deviceErr("destroy_link", code); e != nil {
				firstErr = e
			} else {
				c.destroyed = true
			}
		}
	}

	c.abortMu.Lock()
	abort := c.abort
	c.abort = nil
	c.abortMu.Unlock()
	if abort != nil {
		abort.close()
	}
	if core != nil {
		core.close()
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// durationToMillis 把时长转换为 VXI-11 报文里的毫秒数（uint32）。
func durationToMillis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if ms > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(ms)
}
