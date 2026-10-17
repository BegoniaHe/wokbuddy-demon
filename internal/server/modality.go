// modality.go 请求模态识别与按模态路由（issue #109）。
//
// 聊天请求里的 image_url / input_audio / file part 不再只用于 gateway_hint：
//   - 请求模型声明了能力（input_modalities 可确定）且缺少对应模态 → 默认 400
//     model_capability（附可用模型提示），不静默送上游；
//   - features.multimodal_routing=true 时先在同一 realm 的目录缓存里找支持该
//     模态的候选模型并重写 model，找不到再 400。
//
// 能力未知（目录未收录/缓存冷/模型未声明）一律放行：无法证明不支持就不拦，
// 与 upstream 的 hint 判定同哲学（宁缺勿滥）。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// requestModalities 解析聊天请求体里带有的非文本输入模态（去重、按出现顺序）。
// 畸形 JSON / 非数组 content 一律返回 nil（请求体后续仍会原样送上游，不在这里
// 改变解析语义）。文本是隐含能力，不返回。
func requestModalities(body []byte) []string {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range peek.Messages {
		for _, part := range m.Content {
			mod := upstream.RequestPartModality(part.Type)
			if mod == "" || seen[mod] {
				continue
			}
			seen[mod] = true
			out = append(out, mod)
		}
	}
	return out
}

// modelInfosSnapshot 返回该 realm 的模型目录缓存快照（**只读缓存，不触发上游
// 探测**）：realm=global 用 global 探测缓存，其余用 CN 动态目录。缓存冷 → nil，
// 调用方跳过能力判定。
func (h *Handler) modelInfosSnapshot(realm string) []upstream.ModelInfo {
	if realm == "global" {
		return h.cfg.Upstream.CachedGlobalModelInfos()
	}
	return cachedModelsSnapshot()
}

// findModelInfo 在目录快照里按裸 id 查模型。
func findModelInfo(infos []upstream.ModelInfo, id string) (upstream.ModelInfo, bool) {
	for _, mi := range infos {
		if mi.ID == id {
			return mi, true
		}
	}
	return upstream.ModelInfo{}, false
}

// missingModalities 返回 request 中有、而模型能力声明里没有的模态。
// 模型能力未知（EffectiveInputModalities 为 nil）→ nil（放行）。
func missingModalities(mi upstream.ModelInfo, want []string) []string {
	have := mi.EffectiveInputModalities()
	if len(have) == 0 {
		return nil
	}
	has := map[string]bool{}
	for _, m := range have {
		has[m] = true
	}
	var missing []string
	for _, m := range want {
		if !has[m] {
			missing = append(missing, m)
		}
	}
	return missing
}

// modelsSupporting 返回目录里声明支持全部 want 模态的模型（保持目录顺序）。
func modelsSupporting(infos []upstream.ModelInfo, want []string) []upstream.ModelInfo {
	var out []upstream.ModelInfo
	for _, mi := range infos {
		if len(missingModalities(mi, want)) == 0 {
			out = append(out, mi)
		}
	}
	return out
}

// applyModalityRoute 在选号前做模态能力判定/路由（issue #109）。返回 false 表示
// 已写出 400（请求不进入轮转）。
func (f *chatFlow) applyModalityRoute() bool {
	want := f.reqModalities
	if len(want) == 0 {
		return true // 纯文本请求：既有路由零改动
	}
	infos := f.h.modelInfosSnapshot(f.realm)
	if len(infos) == 0 {
		return true // 目录缓存冷：无法判定，不拦（宁缺勿滥）
	}
	mi, ok := findModelInfo(infos, f.bareModel)
	if !ok {
		return true // 目录未收录（新模型/别名）：交给上游
	}
	missing := missingModalities(mi, want)
	if len(missing) == 0 {
		return true
	}
	// 默认拒绝：明确告诉客户端模型缺哪个模态，并给出几个可用模型。
	if !f.h.multimodalRoutingEnabled() {
		f.rejectModality(mi.ID, missing, infos)
		return false
	}
	// 按模态路由：同 realm 目录里找支持全部模态的候选（首选目录第一条）。
	cands := modelsSupporting(infos, want)
	if len(cands) == 0 {
		f.rejectModality(mi.ID, missing, infos)
		return false
	}
	chosen := cands[0]
	log.Printf("INFO: [server] multimodal routing: model=%s -> %s (requested modalities=%v, acct realm=%s)",
		f.bareModel, chosen.ID, want, f.realm)
	f.rewriteModelForRouting(chosen.ID)
	return true
}

// rejectModality 写 400 model_capability：错误信息明确缺哪个模态，gateway_hint
// 列出同 realm 支持该模态的候选模型（最多 3 个，不编造）。
func (f *chatFlow) rejectModality(model string, missing []string, infos []upstream.ModelInfo) {
	msg := "model \"" + model + "\" does not support " + strings.Join(missing, "/") + " input"
	cands := modelsSupporting(infos, missing)
	hint := ""
	if len(cands) > 0 {
		names := make([]string, 0, 3)
		for _, c := range cands {
			if len(names) == 3 {
				break
			}
			names = append(names, c.ID)
		}
		hint = "models supporting " + strings.Join(missing, "/") + " input: " + strings.Join(names, ", ")
	}
	f.out.Error(http.StatusBadRequest, "model_capability", msg, hint)
	f.st.status = http.StatusBadRequest
	f.st.outcome = reqlog.OutcomeHTTPError
	log.Printf("WARN: [server] model=%s lacks modalities=%v (request rejected before upstream call) hint=%s",
		logfmt.Truncate(model, 80), missing, hint)
}

// rewriteModelForRouting 把出站请求体的 model 重写为候选裸模型名，并同步
// bareModel（选号、粘性、用量维度都以 bareModel 为准）。
func (f *chatFlow) rewriteModelForRouting(bare string) {
	f.body = rewriteModel(f.body, bare)
	f.bareModel = bare
}

// multimodalRoutingEnabled 读取热配置 features.multimodal_routing（默认 false）。
func (h *Handler) multimodalRoutingEnabled() bool {
	return h.loadLive().MultimodalRouting
}

// containsModality 报告模态列表是否包含目标值。
func containsModality(mods []string, want string) bool {
	for _, m := range mods {
		if m == want {
			return true
		}
	}
	return false
}
