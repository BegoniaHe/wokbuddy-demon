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
	data := make([]map[string]any, 0, len(payload.Models))

	for _, raw := range payload.Models {
		data = append(data, map[string]any{
			"model":      raw,
			"busy_level": h.modelBusyLevel(raw, keyUse, keyRealm),
		})
	}

	writeBusyResponse(w, http.StatusOK, "OK", data)
}

// modelBusyLevel 把单个客户端模型名映射为官方六个 busy_level 之一。
//
// 判定顺序：realm 开关 → 隐藏/密钥白名单 → 模型目录存在性 → 模型级冷却 → 可服务账号。
// 目录缓存冷/未知模型一律 unknown，绝不把「不知道」报成 idle。
func (h *Handler) modelBusyLevel(raw string, keyUse *keyUsage, keyRealm string) string {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "unknown"
	}

	realm, bare, err := resolveKeyModel(model, h.requestModelAliases(model, keyRealm), keyRealm)
	if err != nil || bare == "" {
		return "unknown"
	}

	if realm == realmGlobal && (!h.cfg.GlobalEnabled || !auth.GlobalEnabled()) {
		return "unknown"
	}

	if h.busyModelUnavailable(realm, bare, keyUse) {
		return "unavailable"
	}

	if !h.modelKnown(realm, bare) {
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

// modelKnown 只读本地模型目录缓存判断模型是否存在；缓存未加载返回 false。
// 这里刻意忽略目录 TTL：/v2/chat/busy 是只读预检，目录过期不代表模型消失，
// 否则长时间运行的客户端（超过 10min 未再请求 /v1/models）会把已知模型全部
// 报成 unknown。缓存为空（从未成功拉取）仍然按未知处理。
func (h *Handler) modelKnown(realm, model string) bool {
	if realm == realmGlobal {
		infos := h.cfg.Upstream.CachedGlobalModelInfosAnyAge()
		for i := range infos {
			if infos[i].ID == model {
				return true
			}
		}

		return false
	}

	infos := h.cachedModelInfosAnyAge()
	for i := range infos {
		if infos[i].ID == model {
			return true
		}
	}

	return false
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
