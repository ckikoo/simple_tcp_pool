package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
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

func main() {
	// 创建TCP连接池
	pool := NewTCPPool("27.151.159.237:49831", 5)

	// 创建HTTP客户端，使用自定义的传输层
	client := &http.Client{
		Transport: &PoolTransport{Pool: pool},
	}

	// 设置HTTP处理函数
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 代理请求
		req, err := http.NewRequest(r.Method, r.RequestURI, r.Body)
		if err != nil {
			fmt.Printf("err: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
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

	// 启动HTTP服务器
	fmt.Println("HTTP server listening on port 8000")
	if err := http.ListenAndServe(":8000", nil); err != nil {
		panic(err)
	}
}
