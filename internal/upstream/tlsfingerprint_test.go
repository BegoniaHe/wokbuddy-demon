package upstream

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

// clientHello 解析后的 ClientHello 摘要，供指纹与顺序断言使用。
type clientHello struct {
	legacyVersion uint16
	ciphers       []uint16
	extensions    []uint16
	groups        []uint16
	points        []uint8
	sigalgs       []uint16
	paddingLast   bool
	rawLen        int
}

// parseClientHello 解析 uTLS 生成的完整 TLS 记录（record header + handshake）。
func parseClientHello(t *testing.T, raw []byte) clientHello {
	t.Helper()

	body := readClientHelloBody(t, raw)
	hello := clientHello{rawLen: len(raw)}

	if len(body) < 2+32+1 {
		t.Fatalf("ClientHello 过短: %d", len(body))
	}

	hello.legacyVersion = binary.BigEndian.Uint16(body[0:2])
	pos := 2 + 32
	pos += 1 + int(body[pos]) // legacy_session_id
	pos = parseCipherSuites(body, pos, &hello)
	pos += 1 + int(body[pos]) // legacy_compression_methods

	parseClientHelloExtensions(body, pos, &hello)

	hello.paddingLast = len(hello.extensions) > 0 && hello.extensions[len(hello.extensions)-1] == 21

	return hello
}

// readClientHelloBody 校验 TLS handshake 头并返回 ClientHello body。
func readClientHelloBody(t *testing.T, raw []byte) []byte {
	t.Helper()

	if len(raw) < 4 || raw[0] != 1 {
		t.Fatalf("不是 ClientHello handshake: %x", raw[:min(len(raw), 8)])
	}

	handshakeLen := int(raw[1])<<16 | int(raw[2])<<8 | int(raw[3])
	if len(raw) != 4+handshakeLen {
		t.Fatalf("handshake 长度=%d 与实际=%d 不符", handshakeLen, len(raw)-4)
	}

	return raw[4:]
}

func parseCipherSuites(body []byte, pos int, hello *clientHello) int {
	cipherLen := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	pos += 2

	for range cipherLen / 2 {
		hello.ciphers = append(hello.ciphers, binary.BigEndian.Uint16(body[pos:pos+2]))
		pos += 2
	}

	return pos
}

func parseClientHelloExtensions(body []byte, pos int, hello *clientHello) {
	extTotal := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	pos += 2

	end := pos + extTotal
	for pos < end {
		id := binary.BigEndian.Uint16(body[pos : pos+2])
		length := int(binary.BigEndian.Uint16(body[pos+2 : pos+4]))
		data := body[pos+4 : pos+4+length]
		pos += 4 + length

		hello.extensions = append(hello.extensions, id)

		switch id {
		case 0:
			// server_name 由构建方按目标主机写入，这里只要求存在。
		case 10:
			listLen := int(binary.BigEndian.Uint16(data[0:2]))
			for i := range listLen / 2 {
				hello.groups = append(hello.groups, binary.BigEndian.Uint16(data[2+2*i:4+2*i]))
			}
		case 11:
			listLen := int(data[0])
			hello.points = append(hello.points, data[1:1+listLen]...)
		case 13:
			listLen := int(binary.BigEndian.Uint16(data[0:2]))
			for i := range listLen / 2 {
				hello.sigalgs = append(hello.sigalgs, binary.BigEndian.Uint16(data[2+2*i:4+2*i]))
			}
		}
	}
}

// ja3 按标准算法计算 JA3：MD5("version,ciphers,extensions,curves,pointFormats")。
func (h clientHello) ja3() string {
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join([]string{
		strconv.Itoa(int(h.legacyVersion)),
		joinUint16(h.ciphers, "-"),
		joinUint16(h.extensions, "-"),
		joinUint16(h.groups, "-"),
		joinUint8(h.points, "-"),
	}, ","))))
}

// ja4 按 FoxIO 规范计算 JA4：a_b_c。
func (h clientHello) ja4() string {
	// a：协议 + 版本 + SNI + cipher 个数（两位十进制）+ 扩展个数（两位十进制）+ ALPN。
	sni := "i"
	if slices.Contains(h.extensions, 0) {
		sni = "d"
	}

	alpn := "00"

	version := "13"
	if h.legacyVersion < 0x0303 || !slices.Contains(h.extensions, 43) {
		version = "12"
	}

	prefix := "t" + version + sni + fmt.Sprintf("%02d%02d", len(h.ciphers), len(h.extensions)) + alpn

	// b：cipher 十六进制排序后逗号连接，SHA-256 前 12 位。
	cipherHex := make([]string, len(h.ciphers))
	for i, cipher := range h.ciphers {
		cipherHex[i] = fmt.Sprintf("%04x", cipher)
	}

	slices.Sort(cipherHex)

	cipherDigest := digest12(strings.Join(cipherHex, ","))

	// c：扩展（去掉 SNI/ALPN）十六进制排序后逗号连接 + "_" + sigalgs 原序。
	extHex := make([]string, 0, len(h.extensions))
	for _, id := range h.extensions {
		if id == 0 || id == 16 {
			continue
		}

		extHex = append(extHex, fmt.Sprintf("%04x", id))
	}

	slices.Sort(extHex)

	sigHex := make([]string, len(h.sigalgs))
	for i, alg := range h.sigalgs {
		sigHex[i] = fmt.Sprintf("%04x", alg)
	}

	extensionDigest := digest12(strings.Join(extHex, ",") + "_" + strings.Join(sigHex, ","))

	return prefix + "_" + cipherDigest + "_" + extensionDigest
}

func digest12(input string) string {
	sum := sha256.Sum256([]byte(input))

	return hex.EncodeToString(sum[:])[:12]
}

func joinUint16(values []uint16, sep string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(int(value))
	}

	return strings.Join(parts, sep)
}

func joinUint8(values []uint8, sep string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(int(value))
	}

	return strings.Join(parts, sep)
}

func testTLSSpec(t *testing.T) *clientprofile.TLS {
	t.Helper()

	profile := testProfileSet(t).ForRealm("cn")
	if profile == nil || profile.TLS == nil {
		t.Fatal("测试 profile 缺少 tls 段")
	}

	return profile.TLS
}

// TestClientHelloShape 首连 ClientHello 必须与官方抓包同形：18 cipher、10 扩展、
// 指定顺序、groups 29/23/24、无 ALPN/GREASE/证书压缩/PSK。
func TestClientHelloShape(t *testing.T) {
	t.Parallel()

	raw, err := buildClientHello(testTLSSpec(t), "www.workbuddy.cn")
	if err != nil {
		t.Fatalf("buildClientHello: %v", err)
	}

	hello := parseClientHello(t, raw)

	if got, want := hello.ciphers, testTLSSpec(t).CipherSuites; !slices.Equal(got, want) {
		t.Errorf("ciphers=%v want %v", got, want)
	}

	wantExts := []uint16{0, 23, 65281, 10, 11, 35, 13, 51, 45, 43}
	if !slices.Equal(hello.extensions, wantExts) {
		t.Errorf("extensions=%v want %v", hello.extensions, wantExts)
	}

	if !slices.Equal(hello.groups, []uint16{29, 23, 24}) {
		t.Errorf("groups=%v want [29 23 24]", hello.groups)
	}

	if !slices.Equal(hello.points, []uint8{0}) {
		t.Errorf("pointFormats=%v want [0]", hello.points)
	}

	assertNoForbiddenExtensions(t, hello.extensions)
}

// assertNoForbiddenExtensions 断言无 ALPN / 证书压缩 / PSK / padding，且无 GREASE。
func assertNoForbiddenExtensions(t *testing.T, extensions []uint16) {
	t.Helper()

	for _, forbidden := range []uint16{16, 27, 41, 21} {
		if slices.Contains(extensions, forbidden) {
			t.Errorf("不应出现扩展 %d", forbidden)
		}
	}

	for _, id := range extensions {
		if id&0x0f0f == 0x0a0a && byte(id>>8) == byte(id) {
			t.Errorf("出现 GREASE 扩展 0x%04x", id)
		}
	}
}

// TestClientHelloFingerprint 首连 JA3/JA4 必须与 2026-11-04 抓包逐字一致。
func TestClientHelloFingerprint(t *testing.T) {
	t.Parallel()

	raw, err := buildClientHello(testTLSSpec(t), "www.workbuddy.cn")
	if err != nil {
		t.Fatalf("buildClientHello: %v", err)
	}

	hello := parseClientHello(t, raw)

	if got, want := hello.ja3(), "71dc8c533dd919ae9f4963224a4ba8fd"; got != want {
		t.Errorf("JA3=%s want %s", got, want)
	}

	if got, want := hello.ja4(), "t13d181000_5d04281c6031_78e6aca7449b"; got != want {
		t.Errorf("JA4=%s want %s", got, want)
	}
}

// TestClientHelloPadding 长 SNI 触发 BoringSSL 式 padding：扩展 21 追加在最后，
// JA4 的 a 段扩展数变 11、c 段随 padding 改变。
func TestClientHelloPadding(t *testing.T) {
	t.Parallel()

	const longSNI = "www.workbuddy.cn.example.test"

	raw, err := buildClientHello(testTLSSpec(t), longSNI)
	if err != nil {
		t.Fatalf("buildClientHello: %v", err)
	}

	hello := parseClientHello(t, raw)

	if !hello.paddingLast {
		t.Fatalf("extensions=%v want padding(21) last", hello.extensions)
	}

	if got, want := len(hello.extensions), 11; got != want {
		t.Errorf("扩展数=%d want %d", got, want)
	}

	if got, want := hello.ja4(), "t13d181100_5d04281c6031_ef7df7f74e48"; got != want {
		t.Errorf("JA4=%s want %s", got, want)
	}

	if hello.rawLen != 512 {
		t.Errorf("ClientHello 记录长度=%d want 512", hello.rawLen)
	}
}

// TestClientHelloSpecRejectsUnsupported profile 校验之外的兜底：未映射的扩展 ID
// 直接报错，而不是静默生成一个不同的 ClientHello。
func TestClientHelloSpecRejectsUnsupported(t *testing.T) {
	t.Parallel()

	spec := *testTLSSpec(t)
	spec.Extensions = []uint16{0, 99}

	if _, err := clientHelloSpec(&spec, "www.workbuddy.cn"); err == nil {
		t.Fatal("未知扩展 ID 应报错")
	}
}
