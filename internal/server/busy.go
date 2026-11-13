// busy.go 入站 POST /v2/chat/busy：官方客户端的模型忙闲预检（issue #273）。
//
// 只按本地账号池状态计算，不透传上游：global 侧该路径实测 404，且客户端每
// 2.5s 轮询会放大上游调用量。响应严格对齐官方 {code,msg,data:[{model,busy_level}]}。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	busyMaxBodyBytes = 1 << 20
	// busyMaxModels 单次预检模型数上限：官方 renderer 每批最多 50 个。逐个模型
	// 都要解析别名并扫描本地目录，无上限时一个 1MiB 请求可放大成十万次以上扫描。
	busyMaxModels = 50
)

// busyHandler /v2/chat/busy 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) busyHandler(w http.ResponseWriter, r *http.Request) {
	limitRequestBody(w, r, busyMaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeBusyResponse(w, http.StatusRequestEntityTooLarge, "request too large", nil)
			return
		}

		writeBusyResponse(w, http.StatusBadRequest, "invalid request", nil)

		return
	}

	var payload struct {
		Models []string `json:"models"`
	}
	if json.Unmarshal(body, &payload) != nil {
		writeBusyResponse(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}

	if len(payload.Models) > busyMaxModels {
		writeBusyResponse(w, http.StatusBadRequest, fmt.Sprintf("too many models (max %d)", busyMaxModels), nil)
		return
	}

	keyUse := keyUsageFrom(r.Context())
	keyRealm := keyRealmFrom(r.Context())
	aliasOf := h.aliasResolver(keyRealm)
	known := h.modelKnownSet()
	data := make([]map[string]any, 0, len(payload.Models))

	for _, raw := range payload.Models {
		data = append(data, map[string]any{
			"model":      raw,
			"busy_level": h.modelBusyLevel(raw, keyUse, keyRealm, aliasOf, known),
		})
	}

	writeBusyResponse(w, http.StatusOK, "OK", data)
}

// modelBusyLevel 把单个客户端模型名映射为官方六个 busy_level 之一。
//
// 判定顺序：realm 开关 → 隐藏/密钥白名单 → 模型目录存在性 → 模型级冷却 → 可服务账号。
// 目录缓存冷/未知模型一律 unknown，绝不把「不知道」报成 idle。
//
// aliasOf / known 由调用方按整批请求预构建（别名表按 realm 合并一次、目录建成集合
// 一次）：逐模型重建整表 + 线性扫目录在数十模型的轮询里是平方级浪费。
func (h *Handler) modelBusyLevel(
	raw string, keyUse *keyUsage, keyRealm string,
	aliasOf func(string) map[string]string, known map[string]struct{},
) string {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "unknown"
	}

	realm, bare, err := resolveKeyModel(model, aliasOf(model), keyRealm)
	if err != nil || bare == "" {
		return "unknown"
	}

	if realm == realmGlobal && (!h.cfg.GlobalEnabled || !auth.GlobalEnabled()) {
		return "unknown"
	}

	if h.busyModelUnavailable(realm, bare, keyUse) {
		return "unavailable"
	}

	if _, ok := known[realm+"\x00"+bare]; !ok {
		return "unknown"
	}

	return h.busyLevelFromPool(bare, realm)
}

// busyModelUnavailable 模型被部署级白名单/黑名单拒绝，或被命名密钥白名单拒绝。
func (h *Handler) busyModelUnavailable(realm, model string, keyUse *keyUsage) bool {
	if h.modelHidden(realm, model) || !h.modelAllowedByConfig(realm, model) {
		return true
	}

	return keyUse != nil && !h.cfg.Keys.ModelAllowed(keyUse.id, realm+":"+model)
}

// busyLevelFromPool 按模型级冷却与可服务账号给出 busy/congested/idle/unknown。
func (h *Handler) busyLevelFromPool(model, realm string) string {
	blocked := h.cfg.Pool.ModelBlockedForRealm(model, realm)
	switch {
	case blocked.Blocked && blocked.Busy:
		return "busy"
	case blocked.Blocked && blocked.RateLimited:
		return "congested"
	case blocked.Blocked:
		return "unavailable"
	case h.cfg.Pool.ServableForModelRealm(model, realm):
		return "idle"
	default:
		return "unknown"
	}
}

// modelKnownSet 一次性把两个域的模型目录建成集合（busy 端点批量预检用）。
// 逐模型线性扫目录在数十模型的轮询里是平方级；每批只构建一次。
// 目录缓存为空（从未成功拉取）时集合里没有条目，效果与「未加载即未知」一致。
func (h *Handler) modelKnownSet() map[string]struct{} {
	local := h.cachedModelInfosAnyAge()
	known := make(map[string]struct{}, len(local))

	for i := range local {
		known[realmCN+"\x00"+local[i].ID] = struct{}{}
	}

	if h.cfg.Upstream == nil {
		return known
	}

	global := h.cfg.Upstream.CachedGlobalModelInfosAnyAge()
	for i := range global {
		known[realmGlobal+"\x00"+global[i].ID] = struct{}{}
	}

	return known
}

// writeBusyResponse 官方响应信封；data 恒为数组（空列表输出 []，不是 null）。
func writeBusyResponse(w http.ResponseWriter, status int, msg string, data []map[string]any) {
	if data == nil {
		data = []map[string]any{}
	}

	code := 0
	if status != http.StatusOK {
		code = status
	}

	writeJSON(w, status, map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	})
}
