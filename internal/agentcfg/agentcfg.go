// Package agentcfg 本机智能体客户端一键配置（issue #102）。
//
// 只做本机回环模式：检测 Claude Code / Codex CLI / OpenCode 的配置文件，
// 增量写入网关专属段，写前备份首次原件，支持按备份字节级还原。API key 只写入
// 客户端本地配置，不通过面板回显。
package agentcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
)

// ErrModified 当前文件与上次写入内容不一致（用户手改过），需显式 force。
var ErrModified = errors.New("配置文件在上次写入后被修改，需确认覆盖")

// ErrNotApplied 该客户端没有本工具的写入记录。
var ErrNotApplied = errors.New("没有可还原的写入记录")

// Options 写入参数。
var (
	errAPIKey        = errors.New("api_key 不能为空")
	errBaseURLHTTPS  = errors.New("base_url 必须是 http(s) 地址")
	errApplyFailed   = errors.New("Apply failed")
	errRestoreFailed = errors.New("Restore failed")
)

type Options struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

// Status 单个客户端状态。
type Status struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Detected  bool   `json:"detected"`
	Applied   bool   `json:"applied"`
	Modified  bool   `json:"modified"`
	AppliedAt string `json:"applied_at,omitempty"`
}

// Manager 本机配置管理器。
type Manager struct {
	Home    string // 用户主目录
	DataDir string // 备份目录（data/agent-backups）

	// mu 串行化 Apply/Restore/Status 的读-改-写序列。三者都是「读现有配置 →
	// 改 → 写回」或读备份元数据；面板允许并发请求，无锁会丢更新、让后写覆盖
	// 先写，甚至写出与 meta 不一致的状态。
	mu sync.Mutex
}

// New 构造管理器。
func New(home, dataDir string) *Manager { return &Manager{Home: home, DataDir: dataDir} }

type client struct {
	id, name string
	path     func(home string) string
	apply    func(existing []byte, opts Options) ([]byte, error)
}

var clients = []client{
	{
		id: "claude", name: "Claude Code", apply: applyClaude,
		path: func(home string) string { return filepath.Join(home, ".claude", "settings.json") },
	},
	{
		id: "codex", name: "Codex CLI", apply: applyCodex,
		path: func(home string) string { return filepath.Join(home, ".codex", "config.toml") },
	},
	{
		id: "opencode", name: "OpenCode", apply: applyOpenCode,
		path: func(home string) string { return filepath.Join(home, ".config", "opencode", "opencode.json") },
	},
}

type meta struct {
	Existed     bool      `json:"existed"`
	AppliedHash string    `json:"applied_hash"`
	AppliedAt   time.Time `json:"applied_at"`
}

func (*Manager) client(id string) (client, bool) {
	for _, c := range clients {
		if c.id == id {
			return c, true
		}
	}

	return client{}, false
}

func (m *Manager) backupDir(id string) string { return filepath.Join(m.DataDir, id) }
func (m *Manager) metaPath(id string) string  { return filepath.Join(m.backupDir(id), "meta.json") }

func (m *Manager) originalPath(id string) string { return filepath.Join(m.backupDir(id), "original") }

// Status 返回全部客户端状态。
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.statusLocked()
}

// statusLocked 无锁实现；调用方已持 m.mu。
func (m *Manager) statusLocked() []Status {
	out := make([]Status, 0, len(clients))
	for _, spec := range clients {
		path := spec.path(m.Home)

		status := Status{
			ID: spec.id, Name: spec.name, Path: path, Detected: detected(path),
			Applied: false, Modified: false, AppliedAt: "",
		}
		if mt, err := m.readMeta(spec.id); err == nil {
			status.Applied = true

			status.AppliedAt = mt.AppliedAt.Format(time.RFC3339)
			if raw, err := os.ReadFile(path); err == nil {
				status.Modified = hash(raw) != mt.AppliedHash
			} else {
				status.Modified = true
			}
		}

		out = append(out, status)
	}

	return out
}

// validateOptions 校验写入参数并补默认模型。
func validateOptions(opts *Options) error {
	if strings.TrimSpace(opts.APIKey) == "" {
		return errAPIKey
	}

	baseURL := strings.TrimSpace(opts.BaseURL)

	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errBaseURLHTTPS
	}

	opts.BaseURL = baseURL

	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = "glm-5.2"
	}

	return nil
}

// Apply 写入网关专属配置。
//
//nolint:funlen // 配置读改写与回滚线性展开。
func (m *Manager) Apply(id string, opts Options) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	client2, ok := m.client(id)
	if !ok {
		return Status{}, fmt.Errorf("unknown client %q: %w", id, errApplyFailed)
	}

	if err := validateOptions(&opts); err != nil {
		return Status{}, err
	}

	path := client2.path(m.Home)

	existing, existed, err := readFile(path)
	if err != nil {
		return Status{}, err
	}

	mt, err := m.prepareMeta(id, existing, existed)
	if err != nil {
		return Status{}, err
	}

	out, err := client2.apply(existing, opts)
	if err != nil {
		return Status{}, err
	}

	if err := writeFile(path, out); err != nil {
		return Status{}, err
	}

	mt.AppliedHash = hash(out)

	mt.AppliedAt = time.Now()
	if err := m.writeMeta(id, mt); err != nil {
		return Status{}, err
	}

	for _, st := range m.statusLocked() {
		if st.ID == id {
			return st, nil
		}
	}

	return Status{
		ID: id, Name: client2.name, Path: path, Detected: false,
		Applied: false, Modified: false, AppliedAt: "",
	}, nil
}

// prepareMeta 首次写入时备份原件；已有写入记录时校验文件未被外部修改。
func (m *Manager) prepareMeta(id string, existing []byte, existed bool) (meta, error) {
	mt, err := m.readMeta(id)
	if err != nil {
		if err := m.saveOriginal(id, existing, existed); err != nil {
			return meta{}, err
		}

		return meta{Existed: existed}, nil
	}

	if hash(existing) != mt.AppliedHash {
		return meta{}, ErrModified
	}

	return mt, nil
}

// Restore 还原到首次写入前；网关新建的文件会被删除。
func (m *Manager) Restore(id string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client2, ok := m.client(id)
	if !ok {
		return fmt.Errorf("unknown client %q: %w", id, errRestoreFailed)
	}

	mt, err := m.readMeta(id)
	if err != nil {
		return ErrNotApplied
	}

	path := client2.path(m.Home)
	if raw, err := os.ReadFile(path); err == nil && hash(raw) != mt.AppliedHash && !force {
		return ErrModified
	}

	if !mt.Existed {
		return m.removeCreated(id, path)
	}

	return m.restoreOriginal(id, path)
}

// removeCreated 删除网关新建的配置文件与写入记录。
func (m *Manager) removeCreated(id, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removeCreated: %w", err)
	}

	if err := os.Remove(m.metaPath(id)); err != nil {
		return fmt.Errorf("removeCreated: %w", err)
	}

	return nil
}

// restoreOriginal 用首次原件覆盖当前文件并清除写入记录。
func (m *Manager) restoreOriginal(id, path string) error {
	original, err := os.ReadFile(m.originalPath(id))
	if err != nil {
		return fmt.Errorf("读取备份失败: %w", err)
	}

	if err := writeFile(path, original); err != nil {
		return err
	}

	if err := os.Remove(m.metaPath(id)); err != nil {
		return fmt.Errorf("restoreOriginal: %w", err)
	}

	return nil
}

func (m *Manager) saveOriginal(id string, raw []byte, existed bool) error {
	if err := os.MkdirAll(m.backupDir(id), 0o700); err != nil {
		return fmt.Errorf("saveOriginal: %w", err)
	}

	if !existed {
		return nil
	}

	if err := os.WriteFile(m.originalPath(id), raw, 0o600); err != nil {
		return fmt.Errorf("saveOriginal: %w", err)
	}

	return nil
}

func (m *Manager) readMeta(id string) (meta, error) {
	var mt meta

	raw, err := os.ReadFile(m.metaPath(id))
	if err != nil {
		return mt, fmt.Errorf("readMeta: %w", err)
	}

	err = json.Unmarshal(raw, &mt)
	if err != nil {
		return mt, fmt.Errorf("readMeta: %w", err)
	}

	return mt, nil
}

func (m *Manager) writeMeta(id string, mt meta) error {
	raw, err := json.MarshalIndent(mt, "", "  ")
	if err != nil {
		return fmt.Errorf("writeMeta: %w", err)
	}

	if err := os.MkdirAll(m.backupDir(id), 0o700); err != nil {
		return fmt.Errorf("writeMeta: %w", err)
	}

	path := m.metaPath(id)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writeMeta: %w", err)
	}

	if err := atomicwrite.Rename(tmp, path); err != nil {
		return fmt.Errorf("writeMeta: %w", err)
	}

	return nil
}

// loadConfigDoc 解析已有客户端配置；空内容按空对象处理。
func loadConfigDoc(existing []byte, label string) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("现有 %s 配置不是合法 JSON: %w", label, err)
		}

		if doc == nil {
			doc = map[string]any{}
		}
	}

	return doc, nil
}

func applyClaude(existing []byte, opts Options) ([]byte, error) {
	doc, err := loadConfigDoc(existing, "Claude")
	if err != nil {
		return nil, err
	}

	env, _ := doc["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}

	env["ANTHROPIC_BASE_URL"] = gatewayRoot(opts.BaseURL)
	env["ANTHROPIC_AUTH_TOKEN"] = opts.APIKey
	env["ANTHROPIC_MODEL"] = opts.Model
	doc["env"] = env

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("applyClaude: %w", err)
	}

	return raw, nil
}

const (
	codexStart = "# >>> wokbuddy-demon >>>"
	codexEnd   = "# <<< wokbuddy-demon <<<"
)

var (
	codexRootModel         = regexp.MustCompile(`(?m)^[ \t]*model[ \t]*=.*(?:\r?\n|$)`)
	codexRootModelProvider = regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=.*(?:\r?\n|$)`)
)

// gatewayRoot 返回网关根地址：兼容用户误填的 /v1 后缀。
func gatewayRoot(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	return strings.TrimSuffix(base, "/v1")
}

// v1Base 返回 OpenAI 兼容 base_url（Codex / OpenCode 需要 /v1 前缀）。
func v1Base(base string) string {
	root := gatewayRoot(base)
	return root + "/v1"
}

// applyCodex 写入 Codex 配置：根键必须位于首个表头之前，provider 表追加到文末。
// 直接追加到文件末尾会把根键写进已有 [profiles.*] 表（TOML 语义），因此这里
// 先拆出根段再重建，并清掉同名根键避免 duplicate key。
func applyCodex(existing []byte, opts Options) ([]byte, error) {
	text := removeCodexBlock(string(existing))
	root, tables := splitCodexRoot(text)
	root = codexRootModel.ReplaceAllString(root, "")
	root = codexRootModelProvider.ReplaceAllString(root, "")
	root = strings.TrimSpace(root)

	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\nmodel_provider = \"wokbuddy\"\nmodel = %q\n", codexStart, opts.Model)

	if root != "" {
		builder.WriteString("\n")
		builder.WriteString(root)
		builder.WriteString("\n")
	}

	fmt.Fprintf(&builder, `
[model_providers.wokbuddy]
name = "WokBuddy Demon"
base_url = %q
wire_api = "chat"
experimental_bearer_token = %q
%s
`, v1Base(opts.BaseURL), opts.APIKey, codexEnd)

	if tables = strings.TrimSpace(tables); tables != "" {
		builder.WriteString("\n")
		builder.WriteString(tables)
		builder.WriteString("\n")
	}

	return []byte(builder.String()), nil
}

// removeCodexBlock 删除上一次写入的托管段（含标记），其余内容原样保留。
func removeCodexBlock(text string) string {
	start := strings.Index(text, codexStart)
	if start < 0 {
		return text
	}

	end := strings.Index(text[start:], codexEnd)
	if end < 0 {
		return text
	}

	end += start + len(codexEnd)

	return text[:start] + strings.TrimPrefix(text[end:], "\r\n")
}

// splitCodexRoot 以首个 `[table]` 行为界拆分根段与表段。
func splitCodexRoot(text string) (root, tables string) {
	offset := 0

	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			return text[:offset], text[offset:]
		}

		offset += len(line)
	}

	return text, ""
}

func applyOpenCode(existing []byte, opts Options) ([]byte, error) {
	doc, err := loadConfigDoc(existing, "OpenCode")
	if err != nil {
		return nil, err
	}

	provider, _ := doc["provider"].(map[string]any)
	if provider == nil {
		provider = map[string]any{}
	}

	models := map[string]any{opts.Model: map[string]any{}}
	provider["wokbuddy"] = map[string]any{
		"npm":  "@ai-sdk/openai-compatible",
		"name": "WokBuddy Demon",
		"options": map[string]any{
			"baseURL": v1Base(opts.BaseURL),
			"apiKey":  opts.APIKey,
		},
		"models": models,
	}
	doc["provider"] = provider

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("applyOpenCode: %w", err)
	}

	return raw, nil
}

func detected(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	// 未生成配置文件但客户端目录已存在，也视为已安装。
	if st, err := os.Stat(filepath.Dir(path)); err == nil && st.IsDir() {
		return true
	}

	return false
}

func readFile(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, true, nil
	}

	if os.IsNotExist(err) {
		return nil, false, nil
	}

	return nil, false, fmt.Errorf("readFile: %w", err)
}

func writeFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("writeFile: %w", err)
	}

	tmp := path + ".wokbuddy.tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { //nolint:gosec // 路径来自固定客户端定义 + 用户主目录
		return fmt.Errorf("writeFile: %w", err)
	}

	if err := atomicwrite.Rename(tmp, path); err != nil {
		return fmt.Errorf("writeFile: %w", err)
	}

	return nil
}

func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
