package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// TCP连接池
type TCPPool struct {
	mu    sync.Mutex
	conns chan net.Conn
	addr  string
}

// NoCloseConn 是一个包装 net.Conn 的结构，不会在 Close 时关闭底层连接
type NoCloseConn struct {
	mu      sync.Mutex
	conn    net.Conn
	IsClose bool
}

func (c *NoCloseConn) Read(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.IsClose {
		return 0, errors.New("EOF")
	}
	return c.conn.Read(p)
}

func (c *NoCloseConn) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.IsClose {
		return 0, errors.New("client is close")
	}

	return c.conn.Write(p)
}

func (c *NoCloseConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.IsClose {
		c.IsClose = true
		return nil
	}
	return nil
}

// 创建新的TCP连接池
func NewTCPPool(addr string, size int) *TCPPool {
	pool := &TCPPool{
		conns: make(chan net.Conn, size),
		addr:  addr,
	}
	for i := 0; i < size; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			panic(err)
		}
		pool.conns <- conn
	}
	return pool
}

// 获取一个TCP连接
func (p *TCPPool) Get() net.Conn {
	return <-p.conns
}

// 将TCP连接返回池中
func (p *TCPPool) Put(conn net.Conn) {
	p.conns <- conn
}

// 自定义HTTP传输层
type PoolTransport struct {
	Pool *TCPPool
}

func (t *PoolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	conn := t.Pool.Get()
	defer t.Pool.Put(conn)

	// 发送HTTP请求
	err := req.Write(conn)
	if err != nil {
		return nil, err
	}

	// 读取HTTP响应
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}

	// 需要将响应主体读取到内存中，以便连接可以重用
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	// 创建一个新的响应体，包含已经读取的数据
	resp.Body = io.NopCloser(bytes.NewReader(body))

	return resp, nil
}

func handleTunneling(pool *TCPPool, c *gin.Context) {
	r := c.Request
	w := c.Writer

	clientConn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Printf("Hijack error: %v", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	conn := pool.Get()
	noClose := NoCloseConn{
		conn: conn,
	}

	destHost := r.Host
	_, err = fmt.Fprintf(&noClose, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", destHost, destHost)
	if err != nil {
		log.Printf("Error sending CONNECT request to proxy: %v", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(&noClose), r)
	if err != nil {
		log.Printf("Failed to connect to destination through proxy: %v", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	fmt.Printf("resp.StatusCode: %v\n", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		resBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to ReadAll resp Body through proxy: %v", err)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		log.Printf("resp.statuscode != 200 return  %v", string(resBody))
		return
	}

	fmt.Fprintf(clientConn, "%s 200 Connection Established\r\n\r\n", c.Request.Proto)
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := clientConn.Read(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("Error reading from clientConn: %v", err)
				break
			}
			if n > 0 {
				_, writeErr := noClose.Write(buf[:n])
				if writeErr != nil {
					log.Printf("Error writing to proxyClient.conn: %v", writeErr)
					break
				}
			}
		}
		log.Println("Completed client to proxy transfer")
		done <- struct{}{}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			noClose.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := noClose.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				log.Printf("Error reading from proxyClient.conn: %v", err)
				break
			}
			if n > 0 {
				_, writeErr := clientConn.Write(buf[:n])
				if writeErr != nil {
					log.Printf("Error writing to clientConn: %v", writeErr)
					break
				}
			}
		}
		log.Println("Completed proxy to client transfer")
		done <- struct{}{}
	}()

	select {
	case <-done:
		log.Println("Transfer completed")
	case <-time.After(10 * time.Second):
		log.Println("Transfer timed out")
	}
}

func main() {
	// 创建TCP连接池
	pool := NewTCPPool("", 1)

	client := &http.Client{
		Transport: &PoolTransport{Pool: pool},
	}

	router := gin.Default()

	router.GET("/", func(ctx *gin.Context) {
		r := ctx.Request
		w := ctx.Writer
		req, err := http.NewRequest(r.Method, r.RequestURI, r.Body)
		if err != nil {
			fmt.Printf("err: %v\n", err)
			ctx.Status(502)
			ctx.Writer.WriteString(err.Error())
			return
		}

		// 复制头信息
		for k, v := range r.Header {
			req.Header[k] = v
		}

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		// 将响应写回客户端
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	router.NoRoute(func(ctx *gin.Context) {
		r := ctx.Request
		if r.Method == http.MethodConnect {
			handleTunneling(pool, ctx)
		} else {
			ctx.Status(404)
		}
	})

	// 启动HTTP服务器
	fmt.Println("HTTP server listening on port 8000")
	if err := router.Run(":8000"); err != nil {
		panic(err)
	}
}
