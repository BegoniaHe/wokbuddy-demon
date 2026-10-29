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
	errModelList     = errors.New("models 含非法项")
	errApplyFailed   = errors.New("Apply failed")
	errRestoreFailed = errors.New("Restore failed")
)

type Options struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	// Models 非空时覆盖 Model：第 0 个元素作为默认模型（Model 被忽略），
	// 其余按客户端能力写入（OpenCode 全量 / Codex profiles / Claude 仅默认）。
	Models []string `json:"models"`
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

	models, err := normalizeModelList(opts.Models)
	if err != nil {
		return err
	}

	opts.Models = models
	if len(models) > 0 {
		// 归一后非空：列表第一项即默认模型，Model 被忽略。
		opts.Model = models[0]

		return nil
	}

	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = "glm-5.2"
	}

	return nil
}

// maxModelList 模型列表上限：超出截断（不报错），避免把整份目录写进客户端配置。
const maxModelList = 200

// normalizeModelList trim、丢空、去重（保序）、上限截断，并拒绝会写坏 TOML / JSON
// 的控制字符。不做字符集校验：模型 id 由上游决定（含 : . / - 都合法）。
//
// 控制字符一律拒绝：写 Codex 的 config.toml 走 Go 的 %q，它把 \x01 之类输出成
// `\x01`——TOML 不认这种转义，整份配置解析失败。只拦 \r\n\t 不够。
func normalizeModelList(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))

	for _, raw := range list {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}

		if strings.ContainsFunc(id, isControlRune) {
			return nil, fmt.Errorf("%w: 含控制字符 %q 的模型 id 无法写入客户端配置", errModelList, id)
		}

		if _, dup := seen[id]; dup {
			continue
		}

		seen[id] = struct{}{}
		out = append(out, id)

		if len(out) == maxModelList {
			break
		}
	}

	return out, nil
}

// isControlRune 判定 C0/C1 控制字符（含 \r \n \t 与 DEL）。
func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// modelList 三家客户端共用的模型清单：Models 非空取它（已归一），否则回落单模型。
func modelList(opts Options) []string {
	if len(opts.Models) > 0 {
		return opts.Models
	}

	return []string{opts.Model}
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

// applyClaude 只写单个默认模型（opts.Model = 列表第一项）：
// Claude Code 的 settings.json 没有多模型字段，其余模型在 Claude Code 内用
// `/model <id>` 切换，不写进配置。
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
`, v1Base(opts.BaseURL), opts.APIKey)

	// 其余模型写成 profiles：`codex --profile "<id>"` 直接切换。表名必须用双引号
	// 包裹原始 id（模型 id 可含 : . / -）。用户自有 profile 原样保留，只让出与本次
	// 写入同名的键——同名表重复声明会让整份 config.toml 解析失败。
	extra := modelList(opts)[1:]
	for _, id := range extra {
		fmt.Fprintf(&builder, "\n[profiles.%q]\nmodel = %q\nmodel_provider = \"wokbuddy\"\n", id, id)
	}

	builder.WriteString(codexEnd)
	builder.WriteString("\n")

	if tables = strings.TrimSpace(stripCodexProfiles(tables, extra)); tables != "" {
		builder.WriteString("\n")
		builder.WriteString(tables)
		builder.WriteString("\n")
	}

	return []byte(builder.String()), nil
}

// stripCodexProfiles 从表段里删掉与托管 profile 同名的 `[profiles.<id>]` 块（含其
// 键行）。同名表在 TOML 里重复声明是硬错误，Codex 会整份配置加载失败；用户自有的
// 其它 profile 与表原样保留。
func stripCodexProfiles(tables string, ids []string) string {
	if tables == "" || len(ids) == 0 {
		return tables
	}

	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}

	var (
		out   []string
		block []string
		keep  bool
	)

	flush := func() {
		if keep {
			out = append(out, block...)
		}
	}

	for _, line := range strings.SplitAfter(tables, "\n") {
		// 任何表头都结束上一个块：否则被丢弃的 profile 会把紧随其后的
		// 其它表一并吞掉（[model_providers.x] 等与 profile 无关的表不能连坐）。
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			flush()

			id, isProfile := codexProfileHeaderID(line)
			block, keep = []string{line}, !isProfile || !drop[id]

			continue
		}

		block = append(block, line)
	}

	flush()

	return strings.Join(out, "")
}

// codexProfileHeaderID 解析 `[profiles.<id>]` 表头并返回裸 id；其它行返回 false。
func codexProfileHeaderID(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[profiles.") || strings.HasPrefix(trimmed, "[[") {
		return "", false
	}

	end := strings.IndexByte(trimmed, ']')
	if end < 0 {
		return "", false
	}

	key := strings.TrimSpace(trimmed[len("[profiles."):end])
	key = strings.TrimPrefix(strings.TrimSuffix(key, "\""), "\"")
	key = strings.TrimPrefix(strings.TrimSuffix(key, "'"), "'")

	if key == "" {
		return "", false
	}

	return key, true
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

	models := make(map[string]any, len(modelList(opts)))
	for _, id := range modelList(opts) {
		models[id] = map[string]any{}
	}

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
