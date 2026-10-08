// ctrl.go —— hostapd / wpa_supplicant 共用的 ctrl_interface 客户端（unixgram 协议同源）。
// 协议：UNIX 数据报。客户端绑定自己的 socket，向 <dir>/<iface> 发命令（尾部 \n），
// 响应回到自己 socket；ATTACH 后异步事件也走同一路径。
package netctrl

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client hostapd 控制接口客户端
type Client struct {
	conn   *net.UnixConn
	server net.Addr
	local  string // 本端 socket 文件路径（hostapd 回复发到这里，Close 时需删除）
	events chan string

	mu     sync.Mutex // 序列化请求并保护 respCh/closed
	respCh chan string // 在途请求的响应槽：非 nil 时下一条消息视为响应
	closed bool
}

// Dial 连接 ctrl 目录里的服务端 socket（优先接口同名文件，否则取目录里第一个 socket）
func Dial(dir, iface string) (*Client, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sockPath := filepath.Join(dir, iface)
	if st, err := os.Stat(sockPath); err != nil || st.IsDir() {
		entries, _ := os.ReadDir(dir)
		found := ""
		for _, e := range entries {
			if !e.IsDir() {
				found = filepath.Join(dir, e.Name())
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("ctrl 目录 %s 中没有 socket", dir)
		}
		sockPath = found
	}

	local := filepath.Join(dir, fmt.Sprintf("apm-%d-%d", os.Getpid(), rand.Int31()))
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:   conn,
		server: &net.UnixAddr{Name: sockPath, Net: "unixgram"},
		local:  local,
		events: make(chan string, 64),
	}
	go c.readLoop()
	if _, err := c.Request("ATTACH"); err != nil {
		conn.Close()
		os.Remove(local) // 本端 socket 文件必须清理，否则 ctrl 目录下死文件无限堆积
		return nil, fmt.Errorf("ATTACH 失败: %w", err)
	}
	return c, nil
}

// readLoop 持续读；有在途请求时消息作为响应投递，否则作为事件
func (c *Client) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, _, err := c.conn.ReadFrom(buf)
		if err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if closed {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		msg := strings.TrimRight(string(buf[:n]), "\n")
		c.mu.Lock()
		ch := c.respCh
		c.respCh = nil
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
			continue
		}
		select {
		case c.events <- msg:
		default: // 事件积压丢弃（状态轮询兜底）
		}
	}
}

// Request 发送命令并等响应（3s 超时；返回 FAIL 时 err 非 nil）
func (c *Client) Request(cmd string) (string, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", fmt.Errorf("控制接口已关闭")
	}
	if c.respCh != nil {
		c.mu.Unlock()
		return "", fmt.Errorf("并发请求不支持")
	}
	ch := make(chan string, 1)
	c.respCh = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.respCh == ch {
			c.respCh = nil
		}
		c.mu.Unlock()
	}()
	if _, err := c.conn.WriteTo([]byte(cmd+"\n"), c.server); err != nil {
		return "", err
	}
	select {
	case resp := <-ch:
		if strings.HasPrefix(resp, "FAIL") {
			return resp, fmt.Errorf("hostapd 返回 FAIL")
		}
		return resp, nil
	case <-time.After(3 * time.Second):
		return "", fmt.Errorf("hostapd 控制命令 %q 超时", cmd)
	}
}

// Events 事件通道（AP-STA-CONNECTED 等），Close 时关闭
func (c *Client) Events() <-chan string { return c.events }

// Close 关闭客户端 socket 与事件流
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.conn.Close()
	os.Remove(c.local) // 同步删除本端 socket 文件，防泄漏
	close(c.events)
}
