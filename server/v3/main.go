package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
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
		log.Printf("Creating new connection: %v\n", conn.RemoteAddr())
		pool.conns <- conn
	}
	return pool
}

// 获取一个TCP连接
func (p *TCPPool) Get() net.Conn {
	conn := <-p.conns
	if !isConnectionAlive(conn) {
		log.Printf("Connection %v is dead. Recreating...\n", conn.RemoteAddr())
		conn.Close()
		newConn, err := net.Dial("tcp", p.addr)
		if err != nil {
			log.Printf("Failed to create new connection: %v", err)
			return p.Get() // Retry until a valid connection is created
		}
		log.Printf("Created new connection: %v\n", newConn.RemoteAddr())
		return newConn
	}
	log.Printf("Getting connection from pool: %v\n", conn.RemoteAddr())
	return conn
}

// 检查连接是否仍然存活
func isConnectionAlive(conn net.Conn) bool {
	one := []byte{}
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Read(one); err != nil && err != io.EOF {
		return false
	}
	conn.SetReadDeadline(time.Time{})
	return true
}

// 将TCP连接返回池中
func (p *TCPPool) Put(conn net.Conn) {
	if !isConnectionAlive(conn) {
		log.Printf("Connection %v is not alive, closing and not returning to pool.\n", conn.RemoteAddr())
		conn.Close()
		return
	}
	log.Printf("Returning connection to pool: %v\n", conn.RemoteAddr())
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
	log.Printf("Sending HTTP request to %s\n", req.URL)
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

	log.Printf("Received response from %s with status %d\n", req.URL, resp.StatusCode)
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

	destHost := r.Host
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", destHost, destHost)
	if err != nil {
		log.Printf("Error sending CONNECT request to proxy: %v", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), r)
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
		defer conn.Close()
		defer clientConn.Close()
		io.Copy(conn, clientConn)
	}()

	go func() {
		defer conn.Close()
		defer clientConn.Close()
		io.Copy(clientConn, conn)
		log.Println("Completed proxy to client transfer")
		done <- struct{}{}
	}()

	select {
	case <-done:
		log.Println("Transfer completed")
	case <-time.After(10 * time.Second):
		log.Println("Transfer timed out")
	}

	go func() {
		conn, err = net.Dial("tcp", conn.RemoteAddr().String())
		if err != nil {
			return
		}
		pool.Put(conn)
	}()
}

func main() {
	url, err := GetProxyUrlAndPort()
	if err != nil {
		panic(err)
	}

	// 创建TCP连接池
	pool := NewTCPPool(url, 10)

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

func GetProxyUrlAndPort() (string, error) {
	reqUrl := ""
	client := http.Client{}
	req, _ := http.NewRequest("GET", reqUrl, nil)
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	body, _ := io.ReadAll(res.Body)
	newContent := strings.TrimSpace(string(body))
	_, err = url.Parse("http://" + newContent)
	if err != nil {
		return "", err
	}

	return string(newContent), nil
}
