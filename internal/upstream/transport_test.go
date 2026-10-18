package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testAddr 最小 net.Addr 实现，供 deadlineRecorder 使用。
type testAddr string

func (testAddr) Network() string  { return "tcp" }
func (a testAddr) String() string { return string(a) }

// deadlineRecorder 记录 Read/Write 与 deadline 设置次数，验证 writeDeadlineConn
// 只在写方向设 deadline、读方向不设。
type deadlineRecorder struct {
	writes            int
	reads             int
	writeDeadlineSets int
	readDeadlineSets  int
	writeDeadline     time.Time
}

func (c *deadlineRecorder) Read([]byte) (int, error)        { c.reads++; return 0, io.EOF }
func (c *deadlineRecorder) Write(p []byte) (int, error)     { c.writes++; return len(p), nil }
func (c *deadlineRecorder) Close() error                    { return nil }
func (c *deadlineRecorder) LocalAddr() net.Addr             { return testAddr("local") }
func (c *deadlineRecorder) RemoteAddr() net.Addr            { return testAddr("remote") }
func (c *deadlineRecorder) SetDeadline(time.Time) error     { return nil }
func (c *deadlineRecorder) SetReadDeadline(time.Time) error { c.readDeadlineSets++; return nil }
func (c *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	c.writeDeadlineSets++
	c.writeDeadline = t
	return nil
}

func TestWriteDeadlineConnSetsDeadlineOnWrite(t *testing.T) {
	rec := &deadlineRecorder{}
	conn := &writeDeadlineConn{Conn: rec, writeTimeout: 500 * time.Millisecond}

	start := time.Now()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.writeDeadlineSets != 1 || rec.writes != 1 {
		t.Fatalf("writeDeadlineSets=%d writes=%d want 1/1", rec.writeDeadlineSets, rec.writes)
	}
	// deadline 应约为 now+500ms（留宽松窗口避免时钟抖动）。
	if d := rec.writeDeadline.Sub(start); d < 400*time.Millisecond || d > time.Second {
		t.Fatalf("write deadline delta=%v want ~500ms", d)
	}
	// 读方向不设 deadline。
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read should surface underlying io.EOF")
	}
	if rec.readDeadlineSets != 0 || rec.reads != 1 {
		t.Fatalf("readDeadlineSets=%d reads=%d want 0/1", rec.readDeadlineSets, rec.reads)
	}
}

func TestWriteDeadlineConnDisabled(t *testing.T) {
	rec := &deadlineRecorder{}
	conn := &writeDeadlineConn{Conn: rec, writeTimeout: 0}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.writeDeadlineSets != 0 || rec.writes != 1 {
		t.Fatalf("writeDeadlineSets=%d writes=%d want 0/1", rec.writeDeadlineSets, rec.writes)
	}
}

// TestNewTransportWrapsDialedConn 断言 newTransport 的 DialContext 返回包装类型。
func TestNewTransportWrapsDialedConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		<-done
	}()
	defer close(done)

	tr := newTransport()
	conn, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*writeDeadlineConn); !ok {
		t.Fatalf("dialed conn type=%T want *writeDeadlineConn", conn)
	}
}

// TestNewTransportWithWriteTimeoutHealthy 断言健康请求在写 deadline 下正常往返，
// 不产生误杀。
func TestNewTransportWithWriteTimeoutHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 0, Transport: newTransportWithWriteTimeout(2 * time.Second)}
	resp, err := client.Post(srv.URL, "application/json", bytes.NewReader([]byte(`{"a":1}`)))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("status=%d body=%q want 200/ok", resp.StatusCode, body)
	}
}

// TestTransportWriteDeadlineStalledPeer 复现 issue #11：对端接受连接但永不读取
// 请求体时，写阻塞必须在写 deadline 内失败，而非无限挂起。
func TestTransportWriteDeadlineStalledPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		// 收窄接收缓冲，使 TCP 流控在写入极少数据后即阻塞发送方：不依赖
		// body 大小恰好超过内核默认缓冲（autotuning 环境可能把缓冲撑大到
		// 容纳整个 body，导致写不阻塞、测试误等响应头超时）。
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(1 << 10)
		}
		// 永不读取：模拟对端停止消费请求体。
		<-done
	}()
	defer close(done)

	client := &http.Client{Timeout: 0, Transport: newTransportWithWriteTimeout(200 * time.Millisecond)}
	body := bytes.Repeat([]byte("x"), 4<<20) // 远超收端缓冲，写必然阻塞
	start := time.Now()
	resp, err := client.Post("http://"+ln.Addr().String()+"/", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		t.Fatal("Post succeeded against a non-reading peer; want write timeout")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err=%v want a net.Error timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timed out after %v; want well under 5s guard", elapsed)
	}
}
