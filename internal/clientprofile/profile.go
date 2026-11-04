// Package clientprofile 读取部署侧客户端指纹快照（data/client-profiles/<realm>.json）。
//
// profile 只承载「只能从官方 Win32 分发包取到的出站字面量」：UA、IDE 版本、
// 构建 commit/releaseDate。文件存在时整份覆盖对应 realm；仓库不内置任何兜底
// 字面量，缺失由调用方拒绝启动。运行时不按版本号拼装或就地改字段。
package clientprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/safefile"
)

const schemaVersion = 2

var (
	errInvalidProfile    = errors.New("client profile invalid")
	hex64Re              = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex40Re              = regexp.MustCompile(`^[0-9a-f]{40}$`)
	requiredEmitSurfaces = []string{
		"chatUA", "desktopUA", "billingUA", "xIdeVersion", "desktopIdeVersion", "desktopExtVersion",
	}
)

// TLS 扩展 ID；只用于校验 switch，避免魔数（#458）。
const (
	tlsExtServerName             uint16 = 0
	tlsExtALPN                   uint16 = 16
	tlsExtPadding                uint16 = 21
	tlsExtCertificateCompression uint16 = 27
	tlsExtPreSharedKey           uint16 = 41
)

// Source 快照来源：必须是官方 Win32 安装包。
type Source struct {
	Realm          string `json:"realm"`
	PackageURL     string `json:"packageUrl"`
	SHA256         string `json:"sha256"`
	PackageVersion string `json:"packageVersion"`
	Version        string `json:"version"`
	CLIVersion     string `json:"cliVersion"`
	Commit         string `json:"commit"`
	ReleaseDate    int64  `json:"releaseDate"`
	ObservedAt     string `json:"observedAt"`
	GeneratedBy    string `json:"generatedBy"`
}

// Emit 最终出站字面量；loginUA 可为 null。
type Emit struct {
	ChatUA            string  `json:"chatUA"`
	DesktopUA         string  `json:"desktopUA"`
	BillingUA         string  `json:"billingUA"`
	XIdeVersion       string  `json:"xIdeVersion"`
	DesktopIdeVersion string  `json:"desktopIdeVersion"`
	DesktopExtVersion string  `json:"desktopExtVersion"`
	LoginPlatform     string  `json:"loginPlatform"`
	LoginUA           *string `json:"loginUA"`
	// BillingMeterUA billing meter 族（/billing/meter/*）的出站 UA 抓包值：
	// 官方该面没有桌面 UA 拦截器，UA 由 axios 自身默认值补上。**可选**字段——
	// 旧 profile 没有它时为空，出站回落 desktopUA（不自造值、不按版本拼接）。
	BillingMeterUA string `json:"billingMeterUA"`
	// Stainless OpenAI SDK 头族七个值（schema 2 必填，#456）。
	Stainless Stainless `json:"stainless"`
}

// Stainless OpenAI SDK 的 x-stainless-* 头族取值（#456）。官方 CLI chat 全部注入，
// 值来自打包进客户端的 SDK 与运行时，故按构建冻结进 profile。
type Stainless struct {
	Arch           string `json:"arch"`
	Lang           string `json:"lang"`
	OS             string `json:"os"`
	PackageVersion string `json:"packageVersion"`
	RetryCount     string `json:"retryCount"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
}

// Evidence 单个 surface 的来源证据。
type Evidence struct {
	Source  string `json:"source"`
	File    string `json:"file,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Profile 一个 realm 的冻结快照。
type Profile struct {
	Schema   int                 `json:"schema"`
	Source   Source              `json:"source"`
	Emit     Emit                `json:"emit"`
	Missing  []string            `json:"missing"`
	Evidence map[string]Evidence `json:"evidence"`
	// TLS 官方 ClientHello 参数（可选）。只有 upstream.tls_fingerprint=node 时必需；
	// 两个已加载 realm 的该段必须逐字段相等（共享 Transport 无法按 realm 分流）。
	TLS *TLS `json:"tls,omitempty"`
}

// TLS 官方 Node/BoringSSL ClientHello 的数值参数（#458）。顺序即线上顺序：
// Extensions 的顺序就是 ClientHello 里扩展的出现顺序。
type TLS struct {
	Versions            []uint16 `json:"versions"`
	CipherSuites        []uint16 `json:"cipherSuites"`
	Extensions          []uint16 `json:"extensions"`
	Groups              []uint16 `json:"groups"`
	PointFormats        []uint8  `json:"pointFormats"`
	SignatureAlgorithms []uint16 `json:"signatureAlgorithms"`
	PSKModes            []uint8  `json:"pskModes"`
}

// Equal 判断两个 tls 段是否逐字段相等；nil 与 nil 相等。
func (t *TLS) Equal(other *TLS) bool {
	if t == nil || other == nil {
		return t == nil && other == nil
	}

	return slices.Equal(t.Versions, other.Versions) &&
		slices.Equal(t.CipherSuites, other.CipherSuites) &&
		slices.Equal(t.Extensions, other.Extensions) &&
		slices.Equal(t.Groups, other.Groups) &&
		slices.Equal(t.PointFormats, other.PointFormats) &&
		slices.Equal(t.SignatureAlgorithms, other.SignatureAlgorithms) &&
		slices.Equal(t.PSKModes, other.PSKModes)
}

// Set 两个 realm 的 profile 集合；缺失的 realm 返回 nil（不回落到任何内置指纹）。
type Set struct {
	profiles map[string]*Profile
}

// ForRealm 返回该 realm 的 profile；未配置返回 nil。
func (s *Set) ForRealm(realm string) *Profile {
	if s == nil {
		return nil
	}

	return s.profiles[realm]
}

// Empty 是否没有任何 profile。
func (s *Set) Empty() bool { return s == nil || len(s.profiles) == 0 }

// LoadDir 读取 dir 下的 cn.json / global.json；目录或文件缺失视为未配置。
// 任一已存在文件校验失败都返回错误，由调用方决定拒绝启动。
func LoadDir(dir string) (*Set, error) {
	set := &Set{profiles: map[string]*Profile{}}
	if dir == "" {
		return set, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return set, nil
		}

		return nil, fmt.Errorf("read client profiles %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		realm, profile, lerr := loadRealmFile(dir, entry.Name())
		if lerr != nil {
			return nil, lerr
		}

		set.profiles[realm] = profile
	}

	if err := validateRealmTLSEquality(set.profiles); err != nil {
		return nil, err
	}

	return set, nil
}

// validateRealmTLSEquality 已加载 realm 的 tls 段必须逐字段相等：出站 Transport
// 是共享的，无法按 realm 切换 ClientHello；不一致时只能拒绝启动，而不是悄悄挑一个。
func validateRealmTLSEquality(profiles map[string]*Profile) error {
	var first *Profile

	for _, realm := range []string{"cn", "global"} {
		profile := profiles[realm]
		if profile == nil {
			continue
		}

		if first == nil {
			first = profile

			continue
		}

		if !first.TLS.Equal(profile.TLS) {
			return fmt.Errorf("client profile cn/global 的 tls 段不一致（共享 Transport 无法按 realm 分流）: %w",
				errInvalidProfile)
		}
	}

	return nil
}

// loadRealmFile 读取并校验单个 realm 文件，返回文件名推导出的 realm。
func loadRealmFile(dir, name string) (string, *Profile, error) {
	realm := strings.TrimSuffix(name, ".json")
	if realm != "cn" && realm != "global" {
		return "", nil, fmt.Errorf("client profile %s: 未知 realm 文件名（只认 cn.json / global.json）: %w",
			name, errInvalidProfile)
	}

	profile, err := LoadFile(filepath.Join(dir, name))
	if err != nil {
		return "", nil, err
	}

	if profile.Source.Realm != realm {
		return "", nil, fmt.Errorf("client profile %s: source.realm=%q 与文件名不一致: %w",
			name, profile.Source.Realm, errInvalidProfile)
	}

	return realm, profile, nil
}

// LoadFile 读取并严格解析单个 profile 文件（拒绝未知字段与多段 JSON）。
// 读取走 safefile：正文不是合法 JSON 时回落 <path>.bak（与 auth / state 同口径），
// 但语义校验失败不回退，由调用方拒绝启动。
func LoadFile(path string) (*Profile, error) {
	raw, err := safefile.Read(path, nil)
	if err != nil {
		return nil, fmt.Errorf("client profile %s: %w", path, err)
	}

	var profile Profile

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&profile); err != nil {
		return nil, fmt.Errorf("parse client profile %s: %w", path, err)
	}

	if err := ensureEOF(decoder); err != nil {
		return nil, fmt.Errorf("parse client profile %s: %w", path, err)
	}

	if err := profile.Validate(); err != nil {
		return nil, fmt.Errorf("client profile %s: %w", path, err)
	}

	return &profile, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("文件包含多段 JSON: %w", errInvalidProfile)
		}

		return fmt.Errorf("decode trailing JSON: %w", err)
	}

	return nil
}

// Validate 校验结构、Win32 来源与出站值安全边界。不读解包树，因此不校验
// evidence 文件是否存在/命中；那一步由生成方（reverse-workbuddy）完成后才能落盘。
func (p *Profile) Validate() error {
	if p == nil {
		return fmt.Errorf("profile 为空: %w", errInvalidProfile)
	}

	if p.Schema != schemaVersion {
		return fmt.Errorf("schema=%d want %d: %w", p.Schema, schemaVersion, errInvalidProfile)
	}

	if err := validateSource(&p.Source); err != nil {
		return err
	}

	if err := validateEmit(&p.Emit); err != nil {
		return err
	}

	if err := validateCrossVersion(&p.Source, &p.Emit); err != nil {
		return err
	}

	if err := validateMissing(&p.Emit, p.Missing); err != nil {
		return err
	}

	if err := validateEvidence(p); err != nil {
		return err
	}

	return validateTLS(p.TLS)
}

func validateSource(src *Source) error {
	if src.Realm != "cn" && src.Realm != "global" {
		return fmt.Errorf("source.realm=%q: %w", src.Realm, errInvalidProfile)
	}

	if err := validatePackageURL(src.Realm, src.PackageURL); err != nil {
		return err
	}

	if !hex64Re.MatchString(src.SHA256) {
		return fmt.Errorf("source.sha256 必须是 64 位小写 hex: %w", errInvalidProfile)
	}

	if !hex40Re.MatchString(src.Commit) {
		return fmt.Errorf("source.commit 必须是 40 位小写 hex: %w", errInvalidProfile)
	}

	if err := validateSourceVersions(src); err != nil {
		return err
	}

	return validateReleaseDate(src.ReleaseDate)
}

func validateSourceVersions(src *Source) error {
	if src.Version == "" || src.CLIVersion == "" || src.PackageVersion == "" {
		return fmt.Errorf("source.version / cliVersion / packageVersion 不能为空: %w", errInvalidProfile)
	}

	return nil
}

func validateReleaseDate(releaseDate int64) error {
	lower := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	upper := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if releaseDate < lower || releaseDate > upper {
		return fmt.Errorf("source.releaseDate=%d 超出合法区间: %w", releaseDate, errInvalidProfile)
	}

	return nil
}

func validatePackageURL(realm, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("source.packageUrl 非法: %w", errInvalidProfile)
	}

	if parsed.Scheme != "https" {
		return fmt.Errorf("source.packageUrl 必须是 https: %w", errInvalidProfile)
	}

	wantHost := map[string]string{"cn": "download.codebuddy.cn", "global": "download.codebuddy.ai"}[realm]
	if parsed.Host != wantHost {
		return fmt.Errorf("source.packageUrl host=%q want %q（只接受 Win32 安装包）: %w",
			parsed.Host, wantHost, errInvalidProfile)
	}

	if !strings.HasPrefix(parsed.Path, "/workbuddy/saas/win32-x64-user/") {
		return fmt.Errorf("source.packageUrl 不是 workbuddy/saas/win32-x64-user 安装包: %w", errInvalidProfile)
	}

	if !strings.HasSuffix(strings.ToLower(parsed.Path), ".exe") {
		return fmt.Errorf("source.packageUrl 必须指向 Win32 .exe 安装包: %w", errInvalidProfile)
	}

	return nil
}

func validateEmit(emit *Emit) error {
	if err := validateRequiredEmit(emit); err != nil {
		return err
	}

	if err := validateEmitValues(emit); err != nil {
		return err
	}

	return validateStainless(&emit.Stainless)
}

func validateRequiredEmit(emit *Emit) error {
	for _, key := range requiredEmitSurfaces {
		if strings.TrimSpace(valueOf(emit, key)) == "" {
			return fmt.Errorf("emit.%s 不能为空: %w", key, errInvalidProfile)
		}
	}

	// loginUA 非 null 即代表"已取到"：空串会让登录链路当成未配置而拒绝，
	// 与 missing 语义冲突，直接按无效 profile 处理。
	if emit.LoginUA != nil && *emit.LoginUA == "" {
		return fmt.Errorf("emit.loginUA 非 null 时不能为空（取不到请写 null 并列入 missing）: %w", errInvalidProfile)
	}

	return nil
}

func validateEmitValues(emit *Emit) error {
	values := map[string]string{
		"chatUA": emit.ChatUA, "desktopUA": emit.DesktopUA, "billingUA": emit.BillingUA,
		"xIdeVersion": emit.XIdeVersion, "desktopIdeVersion": emit.DesktopIdeVersion,
		"desktopExtVersion": emit.DesktopExtVersion, "loginPlatform": emit.LoginPlatform,
		"billingMeterUA": emit.BillingMeterUA,
	}
	if emit.LoginUA != nil {
		values["loginUA"] = *emit.LoginUA
	}

	for key, value := range values {
		if err := validateEmitValue(key, value); err != nil {
			return err
		}
	}

	return nil
}

// validateStainless x-stainless-* 七值全部必填（#456）：官方 CLI chat 恒发，
// 缺任一项都无法复刻。
func validateStainless(stainless *Stainless) error {
	for key, value := range stainlessValues(stainless) {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("emit.stainless.%s 不能为空: %w", key, errInvalidProfile)
		}

		if err := validateEmitValue("stainless."+key, value); err != nil {
			return err
		}
	}

	return nil
}

// stainlessValues 把结构体字段映射回 JSON 键，供必填与安全边界校验共用。
func stainlessValues(s *Stainless) map[string]string {
	return map[string]string{
		"arch": s.Arch, "lang": s.Lang, "os": s.OS,
		"packageVersion": s.PackageVersion, "retryCount": s.RetryCount,
		"runtime": s.Runtime, "runtimeVersion": s.RuntimeVersion,
	}
}

func validateEmitValue(key, value string) error {
	if value == "" {
		return nil
	}

	if value != strings.TrimSpace(value) {
		return fmt.Errorf("emit.%s 含首尾空白: %w", key, errInvalidProfile)
	}

	if len(value) > 1024 {
		return fmt.Errorf("emit.%s 超过 1024 字节: %w", key, errInvalidProfile)
	}

	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("emit.%s 含控制字符: %w", key, errInvalidProfile)
		}
	}

	return nil
}

func validateCrossVersion(src *Source, emit *Emit) error {
	for key, value := range map[string]string{
		"chatUA": emit.ChatUA, "desktopUA": emit.DesktopUA,
		"xIdeVersion": emit.XIdeVersion, "desktopIdeVersion": emit.DesktopIdeVersion,
	} {
		if !strings.Contains(value, src.Version) {
			return fmt.Errorf("emit.%s 未包含 source.version=%q: %w", key, src.Version, errInvalidProfile)
		}
	}

	// 官方 pluginVersion 恒取 resolveProductVersion()，cn/global 当前都与
	// source.version 同源；desktopIdeVersion 允许未来 platformVersion 独立，故只要求包含。
	if emit.DesktopExtVersion != src.Version {
		return fmt.Errorf("emit.desktopExtVersion=%q 必须等于 source.version=%q: %w",
			emit.DesktopExtVersion, src.Version, errInvalidProfile)
	}

	for key, value := range map[string]string{"chatUA": emit.ChatUA, "desktopUA": emit.DesktopUA} {
		if !strings.Contains(value, src.CLIVersion) {
			return fmt.Errorf("emit.%s 未包含 source.cliVersion=%q: %w", key, src.CLIVersion, errInvalidProfile)
		}
	}

	return nil
}

func validateMissing(emit *Emit, missing []string) error {
	got := make([]string, 0, 2)

	for key, value := range map[string]*string{"loginUA": emit.LoginUA} {
		if value == nil {
			got = append(got, key)
		}
	}

	want := slices.Clone(missing)

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		return fmt.Errorf("missing=%v 与 null surface=%v 不一致: %w", missing, got, errInvalidProfile)
	}

	return nil
}

func validateEvidence(profile *Profile) error {
	if profile.Evidence == nil {
		return fmt.Errorf("evidence 不能为空: %w", errInvalidProfile)
	}

	for _, key := range requiredEvidenceKeys(&profile.Emit) {
		evidence, ok := profile.Evidence[key]
		if !ok {
			return fmt.Errorf("evidence.%s 缺失: %w", key, errInvalidProfile)
		}

		if err := validateEvidenceEntry(key, evidence); err != nil {
			return err
		}
	}

	return nil
}

func requiredEvidenceKeys(emit *Emit) []string {
	keys := slices.Clone(requiredEmitSurfaces)

	// stainless 是 schema 2 的必填组，七个值共用一条证据（SDK bundle / 抓包）。
	keys = append(keys, "stainless")

	if emit.LoginPlatform != "" {
		keys = append(keys, "loginPlatform")
	}

	if emit.LoginUA != nil {
		keys = append(keys, "loginUA")
	}

	// billingMeterUA 是可选面：出现了就必须有证据（抓包 note）。
	if emit.BillingMeterUA != "" {
		keys = append(keys, "billingMeterUA")
	}

	return keys
}

// tlsExtensionAllowed 允许出现在 profile tls.extensions 里的扩展 ID。
// 16 (ALPN)、27 (certificate_compression)、41 (pre_shared_key) 被刻意排除：
// 官方首连不带它们，PSK 由会话恢复在运行时追加。
var tlsExtensionAllowed = map[uint16]bool{
	0: true, 10: true, 11: true, 13: true, 21: true, 23: true,
	35: true, 43: true, 45: true, 51: true, 65281: true,
}

// validateTLS 校验可选的 tls 段结构：只接受已知扩展、无 GREASE、无 ALPN/证书
// 压缩/显式 PSK，padding 若存在必须是最后一个，且各数值列表非空。
func validateTLS(spec *TLS) error {
	if spec == nil {
		return nil
	}

	if err := validateTLSShape(spec); err != nil {
		return err
	}

	return validateTLSExtensions(spec.Extensions)
}

// validateTLSShape 校验非空与 GREASE：五个 uint16 列表共用同一检查。
func validateTLSShape(spec *TLS) error {
	if len(spec.Versions) == 0 || len(spec.CipherSuites) == 0 || len(spec.Extensions) == 0 ||
		len(spec.Groups) == 0 || len(spec.PointFormats) == 0 ||
		len(spec.SignatureAlgorithms) == 0 || len(spec.PSKModes) == 0 {
		return fmt.Errorf("tls 段的 versions/cipherSuites/extensions/groups/pointFormats/"+
			"signatureAlgorithms/pskModes 均不能为空: %w", errInvalidProfile)
	}

	checks := []struct {
		field  string
		values []uint16
	}{
		{"tls.versions", spec.Versions},
		{"tls.cipherSuites", spec.CipherSuites},
		{"tls.groups", spec.Groups},
		{"tls.signatureAlgorithms", spec.SignatureAlgorithms},
		{"tls.extensions", spec.Extensions},
	}

	for _, check := range checks {
		if err := rejectGREASE(check.field, check.values); err != nil {
			return err
		}
	}

	return nil
}

//nolint:gocyclo,cyclop // 扩展 ID 校验是线性 switch。
func validateTLSExtensions(extensions []uint16) error {
	seen := make(map[uint16]bool, len(extensions))
	sni := false

	for i, id := range extensions {
		switch id {
		case tlsExtALPN:
			return fmt.Errorf("tls.extensions 不得包含 ALPN(16): %w", errInvalidProfile)
		case tlsExtCertificateCompression:
			return fmt.Errorf("tls.extensions 不得包含 certificate_compression(27): %w", errInvalidProfile)
		case tlsExtPreSharedKey:
			return fmt.Errorf("tls.extensions 不得包含 pre_shared_key(41)（PSK 由会话恢复追加）: %w", errInvalidProfile)
		}

		if !tlsExtensionAllowed[id] {
			return fmt.Errorf("tls.extensions 含未知扩展 ID %d: %w", id, errInvalidProfile)
		}

		if seen[id] {
			return fmt.Errorf("tls.extensions 含重复扩展 ID %d: %w", id, errInvalidProfile)
		}

		seen[id] = true

		if id == tlsExtServerName {
			sni = true
		}

		if id == tlsExtPadding && i != len(extensions)-1 {
			return fmt.Errorf("tls.extensions 的 padding(21) 必须是最后一个（位置 %d/%d）: %w",
				i, len(extensions), errInvalidProfile)
		}
	}

	if !sni {
		return fmt.Errorf("tls.extensions 必须包含 server_name(0): %w", errInvalidProfile)
	}

	return nil
}

// rejectGREASE 拒绝 GREASE 值（形如 0x0a0a / 0x1a1a）：官方 Node 从不启用 GREASE。
func rejectGREASE(field string, values []uint16) error {
	for _, value := range values {
		if value&0x0f0f == 0x0a0a && value>>8 == value&0xff {
			return fmt.Errorf("%s 含 GREASE 值 0x%04x: %w", field, value, errInvalidProfile)
		}
	}

	return nil
}

func validateEvidenceEntry(key string, evidence Evidence) error {
	switch evidence.Source {
	case "package":
		return validatePackageEvidence(key, evidence)
	case "traffic":
		return validateTrafficEvidence(key, evidence)
	default:
		return fmt.Errorf("evidence.%s.source=%q 未知: %w", key, evidence.Source, errInvalidProfile)
	}
}

func validatePackageEvidence(key string, evidence Evidence) error {
	if evidence.File == "" || evidence.Snippet == "" {
		return fmt.Errorf("evidence.%s 的 package 来源必须给 file 与 snippet: %w", key, errInvalidProfile)
	}

	return validateRelativeEvidenceFile(key, evidence.File)
}

func validateTrafficEvidence(key string, evidence Evidence) error {
	// 抓包来源不依附包内文件：用 note 记录日期与端点即可。
	if strings.TrimSpace(evidence.Note) == "" {
		return fmt.Errorf("evidence.%s 的 traffic 来源必须给 note（日期与端点）: %w", key, errInvalidProfile)
	}

	if evidence.File == "" {
		return nil
	}

	return validateRelativeEvidenceFile(key, evidence.File)
}

func validateRelativeEvidenceFile(key, file string) error {
	if !filepath.IsLocal(filepath.FromSlash(file)) {
		return fmt.Errorf("evidence.%s.file=%q 必须是相对路径: %w", key, file, errInvalidProfile)
	}

	return nil
}

func valueOf(emit *Emit, key string) string {
	switch key {
	case "chatUA":
		return emit.ChatUA
	case "desktopUA":
		return emit.DesktopUA
	case "billingUA":
		return emit.BillingUA
	case "xIdeVersion":
		return emit.XIdeVersion
	case "desktopIdeVersion":
		return emit.DesktopIdeVersion
	case "desktopExtVersion":
		return emit.DesktopExtVersion
	case "loginPlatform":
		return emit.LoginPlatform
	default:
		return ""
	}
}
