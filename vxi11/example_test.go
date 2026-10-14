package vxi11

import (
	"fmt"
	"time"
)

// 本文件是可直接运行的示例（go test 会执行并校验输出）。
// 它用内存里的假仪器代替真实设备，因此不需要任何硬件即可运行：
//
//	go test -run Example -v ./vxi11/

// ExampleDial 演示最常用的闭环：Dial -> Query -> Close。
func ExampleDial() {
	// 启动一个假仪器（真实场景下这里不需要，直接 Dial 仪器 IP 即可）。
	m, err := newMock(mockConfig{
		responses: map[string]string{
			"*IDN?": "MOCK,DS1000Z,E12345,1.00\n",
		},
		maxRecvSize: 4096,
	})
	if err != nil {
		fmt.Println("启动假仪器失败:", err)
		return
	}
	defer m.Close()

	// 连接仪器：先查 portmapper 找到 DEVICE_CORE 的端口，再发 create_link。
	// device 通常是 "inst0"。真实代码里 host 换成仪器 IP，例如 "5.5.5.1"。
	c, err := Dial("127.0.0.1", "inst0", 5*time.Second,
		WithPortmapperPort(m.port), // 仅示例需要（假仪器的 portmapper 不在 111 端口）
	)
	if err != nil {
		fmt.Println("Dial 失败:", err)
		return
	}
	defer c.Close() // Close 会发送 destroy_link 并关闭连接

	fmt.Printf("已连接 %s，lid=%d，maxRecvSize=%d\n", c.Host(), c.LinkID(), c.MaxRecvSize())

	// Query 内部完成 write + 分片 read，并去掉结尾的 CR/LF。
	idn, err := c.Query("*IDN?")
	if err != nil {
		fmt.Println("Query 失败:", err)
		return
	}
	fmt.Println("仪器识别:", idn)

	// 直接发送一条不需要应答的命令。
	if err := c.WriteString(":RUN"); err != nil { // 未带换行会自动补 '\n'
		fmt.Println("WriteString 失败:", err)
		return
	}

	// Output:
	// 已连接 127.0.0.1，lid=1，maxRecvSize=4096
	// 仪器识别: MOCK,DS1000Z,E12345,1.00
}

// ExampleClient_QueryBytes 演示二进制安全的读取（不设置终止符），
// 适用于 :DISPLAY:DATA? 这类 IEEE 488.2 定长块应答，返回的长数据会被自动拼接。
func ExampleClient_QueryBytes() {
	block := "#15hello" // IEEE 488.2 定长块：# + 1 位长度 + 长度值 + 数据
	m, err := newMock(mockConfig{
		maxRecvSize: 1024,
		responses:   map[string]string{":DISP:DATA?": block},
	})
	if err != nil {
		fmt.Println("启动假仪器失败:", err)
		return
	}
	defer m.Close()

	c, err := Dial("127.0.0.1", "inst0", 5*time.Second, WithPortmapperPort(m.port))
	if err != nil {
		fmt.Println("Dial 失败:", err)
		return
	}
	defer c.Close()

	data, err := c.QueryBytes(":DISP:DATA?\n", 5*time.Second)
	if err != nil {
		fmt.Println("QueryBytes 失败:", err)
		return
	}
	fmt.Printf("收到 %d 字节: %q\n", len(data), data)

	// Output:
	// 收到 8 字节: "#15hello"
}
