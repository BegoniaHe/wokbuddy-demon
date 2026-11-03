package upstream

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// countingListener 记录每次 Accept 的远端地址，用来判断同一 Transport
// 上的连续请求有没有复用 TCP。
type countingListener struct {
	net.Listener

	mu    sync.Mutex
	addrs []string
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("countingListener.Accept: %w", err)
	}

	l.mu.Lock()
	l.addrs = append(l.addrs, conn.RemoteAddr().String())
	l.mu.Unlock()

	return conn, nil
}

func (l *countingListener) addrsCopy() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]string, len(l.addrs))
	copy(out, l.addrs)

	return out
}

// keepAliveServer 用原始 HTTP/1.1 回包（不发 Connection: close），并留下请求头字节。
// 不能用 httptest：它的 Handler 看不到 transferWriter 写出的 Connection 行，
// 而 req.Header.Get("Connection") 在只设 req.Close 时仍是空的。
type keepAliveServer struct {
	ln    *countingListener
	mu    sync.Mutex
	wires []string
}

func newKeepAliveServer(t *testing.T) *keepAliveServer {
	t.Helper()

	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &keepAliveServer{ln: &countingListener{Listener: base}}

	go srv.serve()

	t.Cleanup(func() { _ = base.Close() })

	return srv
}

func (s *keepAliveServer) url() string { return "http://" + s.ln.Addr().String() }

func (s *keepAliveServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}

		go s.serveConn(conn)
	}
}

func (s *keepAliveServer) serveConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	for {
		raw, err := readHeaderBlock(reader)
		if raw != "" {
			s.mu.Lock()
			s.wires = append(s.wires, raw)
			s.mu.Unlock()
		}

		if err != nil {
			return
		}

		const body = `{"code":0,"data":{"models":[{"id":"m1","name":"m1"}]}}`

		_, err = fmt.Fprintf(conn,
			"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		if err != nil {
			return
		}
	}
}

func (s *keepAliveServer) wiresCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.wires))
	copy(out, s.wires)

	return out
}

func readHeaderBlock(reader *bufio.Reader) (string, error) {
	var block strings.Builder

	for {
		line, err := reader.ReadString('\n')
		block.WriteString(line)

		if err != nil {
			return block.String(), fmt.Errorf("readHeaderBlock: %w", err)
		}

		if line == "\r\n" || line == "\n" {
			return block.String(), nil
		}

		if block.Len() > 64<<10 {
			return block.String(), fmt.Errorf("readHeaderBlock: %w", io.ErrShortBuffer)
		}
	}
}

func connectionLines(raw string) []string {
	var out []string

	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) >= len("connection:") && strings.EqualFold(line[:len("connection:")], "connection:") {
			out = append(out, line)
		}
	}

	return out
}

// TestV3ConfigClosesConnection 连续两次 GET /v3/config 必须各开一条 TCP，
// 且线上字节含且仅含一行 Connection: close。
func TestV3ConfigClosesConnection(t *testing.T) {
	t.Parallel()

	srv := newKeepAliveServer(t)
	client := newProfileClient(t)
	client.ChatBaseCN = srv.url()

	t.Cleanup(func() {
		client.HTTP.CloseIdleConnections()
	})

	account := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.cn"}

	for range 2 {
		got, err := client.fetchV3ConfigModelMap(account, "")
		if err != nil {
			t.Fatalf("fetchV3ConfigModelMap: %v", err)
		}

		if _, ok := got["m1"]; !ok {
			t.Fatalf("models=%v want m1", got)
		}
	}

	addrs := srv.ln.addrsCopy()
	if len(addrs) != 2 || addrs[0] == addrs[1] {
		t.Fatalf("TCP accepts=%v want 2 个不同远端（两次 /v3/config 不得复用连接）", addrs)
	}

	wires := srv.wiresCopy()
	if len(wires) != 2 {
		t.Fatalf("captured wires=%d want 2", len(wires))
	}

	for i, raw := range wires {
		lines := connectionLines(raw)
		if len(lines) != 1 || !strings.EqualFold(lines[0], "Connection: close") {
			t.Errorf("wire %d Connection 行=%q want 恰好一行 Connection: close\n%s", i, lines, raw)
		}
	}
}

// TestOtherSurfacesOmitConnection chat / billing / report / growth 不发 Connection，
// 同一 Transport 上连续请求应复用一条 TCP。
func TestOtherSurfacesOmitConnection(t *testing.T) {
	t.Parallel()

	srv := newKeepAliveServer(t)
	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	t.Cleanup(func() {
		client.HTTP.CloseIdleConnections()
	})

	calls := []struct {
		name string
		do   func(req *http.Request)
	}{
		{"chat", func(req *http.Request) { client.ChatHeaders(req, account, "", ChatMeta{}) }},
		{"billing", func(req *http.Request) { client.BillingHeaders(req, account) }},
		{"report", func(req *http.Request) { client.ReportHeaders(req, account) }},
		{"growth", func(req *http.Request) { client.GrowthHeaders(req, account) }},
	}

	for _, call := range calls {
		req, err := http.NewRequest(http.MethodPost, srv.url()+"/probe/"+call.name, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		call.do(req)

		resp, err := client.HTTP.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if addrs := srv.ln.addrsCopy(); len(addrs) != 1 {
		t.Fatalf("TCP accepts=%v want 1（chat/billing/report/growth 应复用连接）", addrs)
	}

	for i, raw := range srv.wiresCopy() {
		if lines := connectionLines(raw); len(lines) != 0 {
			t.Errorf("wire %d 不应含 Connection，got %q\n%s", i, lines, raw)
		}
	}
}
