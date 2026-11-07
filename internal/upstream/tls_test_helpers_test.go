package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// generateTestCert 生成 127.0.0.1/localhost 的自签证书，供本地 TLS 监听器使用。
func generateTestCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wokbuddy-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost", "www.workbuddy.cn"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// rawRecorder 记录 TLS 解密后的明文读取字节（服务端视角）。
type rawRecorder struct {
	buf bytes.Buffer
}

func (r *rawRecorder) write(p []byte) {
	// http.Server 的读循环单协程，无需加锁；保留方法以便将来并发用例显式加锁。
	r.buf.Write(p)
}

func (r *rawRecorder) snapshot() []byte {
	return append([]byte(nil), r.buf.Bytes()...)
}

// recordingListener 包装 TLS listener，Accept 返回加密连接；记录的是其 Read
// 解密后的明文。
type recordingListener struct {
	net.Listener

	rec *rawRecorder
}

func (l recordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept: %w", err)
	}

	return &recordingConn{Conn: conn, rec: l.rec}, nil
}

type recordingConn struct {
	net.Conn

	rec *rawRecorder
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.rec.write(p[:n])
	}

	return n, err
}

// startRecordingTLSServer 启动本地 TLS 1.3 服务并返回地址、证书与明文记录器。
func startRecordingTLSServer(t *testing.T, handler http.Handler) (string, tls.Certificate, *rawRecorder) {
	t.Helper()

	cert, _ := generateTestCert(t)

	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	rec := &rawRecorder{}
	tlsListener := tls.NewListener(base, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = server.Serve(recordingListener{Listener: tlsListener, rec: rec}) }()

	t.Cleanup(func() {
		_ = server.Close()
		_ = base.Close()
	})

	return base.Addr().String(), cert, rec
}

// testFingerprintClient 返回挂载测试 profile、指向本地 TLS 服务、启用自定义 TLS
// 拨号的 Client。
func testFingerprintClient(t *testing.T, addr string, leaf *x509.Certificate) *Client {
	t.Helper()

	client := newProfileClient(t)
	client.tlsRootCAs = testRootPool(leaf)
	client.SetHeaderOrder(true)
	client.SetDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, network, addr)
	})

	return client
}

// doChat 发一条 CLI chat 请求并读完响应。
func doChat(t *testing.T, client *Client, endpoint, body string) {
	t.Helper()

	doChatWith(t, client, endpoint, body, false)
}

// doChatClose 发一条要求关闭连接的 CLI chat 请求（强制下一次请求新建连接）。
func doChatClose(t *testing.T, client *Client, endpoint, body string) {
	t.Helper()

	doChatWith(t, client, endpoint, body, true)
}

func doChatWith(t *testing.T, client *Client, endpoint, body string, closeConn bool) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	req.Close = closeConn

	client.ChatHeaders(req, surfaceGoldenAccount(), "", ChatMeta{
		ConversationID:        "0f0e0d0c-0b0a-4908-8706-050403020100",
		ConversationRequestID: "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93",
	})

	resp, err := client.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do chat: %v", err)
	}

	defer resp.Body.Close()

	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// writeRecorder 记录客户端发来的原始 TLS 记录字节（未解密）。服务端通过 Read
// 接收客户端写入，故记录 Read 而不是 Write。
type writeRecorder struct {
	net.Conn

	buf bytes.Buffer
}

func (c *writeRecorder) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.buf.Write(p[:n])
	}

	return n, err
}

func (c *writeRecorder) snapshot() []byte {
	return append([]byte(nil), c.buf.Bytes()...)
}

// captureStore 按 Accept 顺序保存每条连接的客户端写入。
type captureStore struct {
	mu    sync.Mutex
	conns []*writeRecorder
}

func (s *captureStore) add(conn *writeRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.conns = append(s.conns, conn)
}

func (s *captureStore) snapshot() []*writeRecorder {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*writeRecorder(nil), s.conns...)
}

// startRawTLSServer 启动一个最小 TLS 1.3 服务：记录客户端原始记录字节，按 HTTP/1.1
// 读写两个往返。会话票由 crypto/tls 服务端默认下发。
func startRawTLSServer(t *testing.T) (string, tls.Certificate, *captureStore) {
	t.Helper()

	cert, _ := generateTestCert(t)

	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	store := &captureStore{}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}

	go func() {
		for {
			raw, aerr := base.Accept()
			if aerr != nil {
				return
			}

			recorder := &writeRecorder{Conn: raw}
			store.add(recorder)

			go serveRawTLSConn(recorder, config)
		}
	}()

	t.Cleanup(func() {
		for _, conn := range store.snapshot() {
			_ = conn.Close()
		}

		_ = base.Close()
	})

	return base.Addr().String(), cert, store
}

func serveRawTLSConn(raw *writeRecorder, config *tls.Config) {
	tlsConn := tls.Server(raw, config)
	if err := tlsConn.Handshake(); err != nil {
		_ = raw.Close()

		return
	}

	defer tlsConn.Close()

	reader := bufio.NewReader(tlsConn)

	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}

		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()

		body := `{"ok":true}`
		resp := &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}

		if err := resp.Write(tlsConn); err != nil {
			return
		}

		if req.Close {
			return
		}
	}
}

// tlsRecords 把原始 TLS 字节切成记录，返回 (type, payload)。
func tlsRecords(t *testing.T, raw []byte) [][2][]byte {
	t.Helper()

	var out [][2][]byte

	for len(raw) > 0 {
		if len(raw) < 5 {
			t.Fatalf("截断的 TLS 记录头: %x", raw)
		}

		length := int(binary.BigEndian.Uint16(raw[3:5]))
		if len(raw) < 5+length {
			t.Fatalf("截断的 TLS 记录体: need %d have %d", 5+length, len(raw))
		}

		out = append(out, [2][]byte{{raw[0]}, raw[5 : 5+length]})
		raw = raw[5+length:]
	}

	return out
}

// firstClientHello 提取连接上第一段 ClientHello handshake 字节（可跨多个记录）。
func firstClientHello(t *testing.T, raw []byte) []byte {
	t.Helper()

	var hello []byte

	for _, record := range tlsRecords(t, raw) {
		if len(record[0]) == 0 || record[0][0] != 22 {
			continue
		}

		hello = append(hello, record[1]...)
		if len(hello) < 4 {
			continue
		}

		need := 4 + (int(hello[1])<<16 | int(hello[2])<<8 | int(hello[3]))
		if len(hello) >= need {
			return hello[:need]
		}
	}

	t.Fatal("未捕获到 ClientHello")

	return nil
}

// hasRecordType 判断原始字节里是否出现指定 TLS 记录类型。
func hasRecordType(t *testing.T, raw []byte, recordType byte) bool {
	t.Helper()

	for _, record := range tlsRecords(t, raw) {
		if len(record[0]) == 1 && record[0][0] == recordType {
			return true
		}
	}

	return false
}

// testRootPool 由测试自签证书构造根池。
func testRootPool(leaf *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return pool
}
