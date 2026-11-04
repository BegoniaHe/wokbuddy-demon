// tlsfingerprint.go 把 profile 的 tls 段编译成 uTLS ClientHelloSpec，并按连接重建
// 每次握手的扩展列表（SNI 与 padding 长度都随目标主机变化）。
//
// 这里只负责「形状」：cipher / 扩展顺序 / 曲线 / sigalgs / 版本。握手本身仍由
// uTLS（crypto/tls 的 fork）完成，证书校验、PSK 恢复、记录层都是真实实现。
package upstream

import (
	"errors"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

// TLS 扩展 ID；只用于 spec 分派，避免魔数（#458）。
const (
	tlsExtServerName           uint16 = 0
	tlsExtSupportedGroups      uint16 = 10
	tlsExtPointFormats         uint16 = 11
	tlsExtSignatureAlgorithms  uint16 = 13
	tlsExtPadding              uint16 = 21
	tlsExtExtendedMasterSecret uint16 = 23
	tlsExtSessionTicket        uint16 = 35
	tlsExtSupportedVersions    uint16 = 43
	tlsExtPSKModes             uint16 = 45
	tlsExtKeyShare             uint16 = 51
	tlsExtRenegotiation        uint16 = 65281
)

var (
	errEmptyTLSSpec            = errors.New("tls 段为空")
	errUnsupportedTLSExtension = errors.New("未支持的 tls 扩展 ID")
)

// clientHelloSpec 按 profile tls 段生成指定 SNI 的 uTLS spec。
//
// 每次握手都重建：SNI 是连接级参数，padding 长度由 BoringPaddingStyle 在
// 序列化时按 ClientHello 长度决定，二者都不能在进程级共享。
func clientHelloSpec(spec *clientprofile.TLS, serverName string) (*utls.ClientHelloSpec, error) {
	if spec == nil {
		return nil, errEmptyTLSSpec
	}

	extensions := make([]utls.TLSExtension, 0, len(spec.Extensions))

	for _, id := range spec.Extensions {
		extension, err := tlsExtensionFor(id, spec, serverName)
		if err != nil {
			return nil, err
		}

		extensions = append(extensions, extension)
	}

	// uTLS 在启用会话恢复时要求 spec 里存在 pre_shared_key 扩展占位；未命中会话时
	// Len()=0，线上不会多出该扩展（首连仍是 10 个扩展）。放到最后也保证恢复时
	// 官方顺序 ...43,21,41。profile 不写 41，避免把运行时状态误当成固定指纹。
	extensions = append(extensions, &utls.UtlsPreSharedKeyExtension{})

	minVersion, maxVersion := spec.Versions[0], spec.Versions[0]
	for _, version := range spec.Versions[1:] {
		minVersion = min(minVersion, version)
		maxVersion = max(maxVersion, version)
	}

	return &utls.ClientHelloSpec{
		CipherSuites:       append([]uint16(nil), spec.CipherSuites...),
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
		TLSVersMin:         minVersion,
		TLSVersMax:         maxVersion,
	}, nil
}

// tlsExtensionFor 把 profile 里的扩展 ID 编译成 uTLS 扩展。分派是一张稳定的
// 线性表，拆成 map 反而更难核对扩展顺序。
//
//nolint:gocyclo,cyclop,ireturn // 扩展 ID 分派是线性 switch；返回 uTLS 扩展接口是其 API 形状。
func tlsExtensionFor(id uint16, spec *clientprofile.TLS, serverName string) (utls.TLSExtension, error) {
	switch id {
	case tlsExtServerName:
		return &utls.SNIExtension{ServerName: serverName}, nil
	case tlsExtSupportedGroups:
		groups := make([]utls.CurveID, len(spec.Groups))
		for i, group := range spec.Groups {
			groups[i] = utls.CurveID(group)
		}

		return &utls.SupportedCurvesExtension{Curves: groups}, nil
	case tlsExtPointFormats:
		return &utls.SupportedPointsExtension{SupportedPoints: spec.PointFormats}, nil
	case tlsExtSignatureAlgorithms:
		sigalgs := make([]utls.SignatureScheme, len(spec.SignatureAlgorithms))
		for i, alg := range spec.SignatureAlgorithms {
			sigalgs[i] = utls.SignatureScheme(alg)
		}

		return &utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: sigalgs}, nil
	case tlsExtPadding:
		return &utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle}, nil
	case tlsExtExtendedMasterSecret:
		return &utls.ExtendedMasterSecretExtension{}, nil
	case tlsExtSessionTicket:
		return &utls.SessionTicketExtension{}, nil
	case tlsExtSupportedVersions:
		return &utls.SupportedVersionsExtension{Versions: spec.Versions}, nil
	case tlsExtPSKModes:
		return &utls.PSKKeyExchangeModesExtension{Modes: spec.PSKModes}, nil
	case tlsExtKeyShare:
		// BoringSSL 首连只带一个 X25519 key share；profile 的 groups 首个即其选择。
		return &utls.KeyShareExtension{
			KeyShares: []utls.KeyShare{{Group: utls.CurveID(spec.Groups[0])}},
		}, nil
	case tlsExtRenegotiation:
		return &utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, nil
	default:
		return nil, fmt.Errorf("%w %d", errUnsupportedTLSExtension, id)
	}
}

// buildClientHello 生成一次真实的首连 ClientHello 报文（不含任何会话恢复），
// 供测试核对扩展顺序、JA3/JA4 与 padding 行为。
func buildClientHello(spec *clientprofile.TLS, serverName string) ([]byte, error) {
	helloSpec, err := clientHelloSpec(spec, serverName)
	if err != nil {
		return nil, err
	}

	conn := &nopConn{}

	uconn := utls.UClient(conn, &utls.Config{ServerName: serverName, OmitEmptyPsk: true}, utls.HelloCustom)
	if err := uconn.ApplyPreset(helloSpec); err != nil {
		return nil, fmt.Errorf("ApplyPreset: %w", err)
	}

	// 不加载会话：首连形状（无 pre_shared_key）。
	if err := uconn.BuildHandshakeStateWithoutSession(); err != nil {
		return nil, fmt.Errorf("BuildHandshakeState: %w", err)
	}

	return append([]byte(nil), uconn.HandshakeState.Hello.Raw...), nil
}

// nopConn 是 ClientHello 构造阶段占位的 net.Conn：BuildHandshakeState 只做序列化，
// 不读写底层连接。
type nopConn struct{}

func (*nopConn) Read([]byte) (int, error)           { return 0, net.ErrClosed }
func (*nopConn) Write(p []byte) (int, error)        { return len(p), nil }
func (*nopConn) Close() error                       { return nil }
func (*nopConn) LocalAddr() net.Addr                { return nopAddr{} }
func (*nopConn) RemoteAddr() net.Addr               { return nopAddr{} }
func (*nopConn) SetDeadline(_ time.Time) error      { return nil }
func (*nopConn) SetReadDeadline(_ time.Time) error  { return nil }
func (*nopConn) SetWriteDeadline(_ time.Time) error { return nil }

type nopAddr struct{}

func (nopAddr) Network() string { return "nop" }
func (nopAddr) String() string  { return "nop" }
