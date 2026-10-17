// modality.go 模型输入模态能力（issue #109）。
//
// 上游模型对象此前只用 supportsImages 一个布尔描述多模态能力；音频/文件等 part
// 无法识别，也无法在候选模型间按模态过滤。这里把能力收敛成结构化的
// input_modalities（至少 text/image/audio/file），来源优先级：
//
//  1. 上游模型对象显式字段（inputModalities 数组，或 modalities.input）；
//  2. models.dev 已加载文档的 modalities.input（进程内索引，不发网络）；
//  3. 现有 supportsImages 兼容推导（true → text+image）。
//
// 三级都拿不到 → nil = **未知**：不宣称、也不做能力拒绝（宁缺勿滥，避免把
// 合法请求拦在目录缺失上）。纯文本是 chat 模型的隐含能力，但「声称 text-only」
// 会误拒真实支持 audio/file 的新模型，故未知时不推导。
package upstream

import "strings"

// 输入模态取值（OpenAI/主流网关通用命名）。
const (
	ModalityText  = "text"
	ModalityImage = "image"
	ModalityAudio = "audio"
	ModalityFile  = "file"
	ModalityVideo = "video"
)

// knownModalities 允许对外宣称的模态集合：过滤上游脏值/未知新值。
var knownModalities = map[string]bool{
	ModalityText:  true,
	ModalityImage: true,
	ModalityAudio: true,
	ModalityFile:  true,
	ModalityVideo: true,
}

// normalizeModalities 归一化模态列表（小写、去重、过滤未知值，保持首次出现顺序）。
func normalizeModalities(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		m := strings.ToLower(strings.TrimSpace(raw))
		if m == "" || !knownModalities[m] || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DeclaredInputModalities 返回上游字段或 models.dev 明确声明的输入模态；nil =
// 没有完整声明。只有完整声明才能用于「缺少某模态 → 拒绝」，因为
// supports_images 只能证明图片能力，不能证明音频/文件缺失。
func (m ModelInfo) DeclaredInputModalities() []string {
	if mods := normalizeModalities(m.InputModalities); len(mods) > 0 {
		return mods
	}
	if mods, ok := lookupModelsDevModalities(m.ID); ok {
		return normalizeModalities(mods)
	}
	return nil
}

// EffectiveInputModalities 返回模型的有效输入模态；nil = 能力未知（不宣称、
// 不做能力拒绝）。来源见文件头三级优先级。
func (m ModelInfo) EffectiveInputModalities() []string {
	if mods := m.DeclaredInputModalities(); len(mods) > 0 {
		return mods
	}
	if m.SupportsImages {
		// 兼容推导：supports_images=true 即支持图片输入（文本恒隐含）。
		return []string{ModalityText, ModalityImage}
	}
	return nil
}

// SupportsModality 报告模型是否声明支持某个输入模态。能力未知（EffectiveInput
// Modalities 为 nil）时返回 true——无法证明不支持就不拒绝（宁缺勿滥）。
func (m ModelInfo) SupportsModality(modality string) bool {
	mods := m.EffectiveInputModalities()
	if len(mods) == 0 {
		return true
	}
	for _, m2 := range mods {
		if m2 == modality {
			return true
		}
	}
	return false
}

// RequestPartModality 把 OpenAI 消息 content part 的 type 映射为输入模态；
// 未知/文本 part 返回空串（文本是隐含能力，不参与能力拒绝与路由）。
//
// 覆盖 OpenAI Chat Completions 与 Responses 两种命名：
//   - image_url / input_image → image
//   - input_audio / audio     → audio
//   - file / input_file       → file
func RequestPartModality(partType string) string {
	switch strings.ToLower(strings.TrimSpace(partType)) {
	case "image_url", "input_image":
		return ModalityImage
	case "input_audio", "audio":
		return ModalityAudio
	case "file", "input_file":
		return ModalityFile
	default:
		return ""
	}
}

// firstNonEmptyModalities 取第一个非空的模态列表（上游两种字段形态的优先级：
// 扁平 inputModalities 优先，缺失回落 modalities.input）。
func firstNonEmptyModalities(lists ...[]string) []string {
	for _, l := range lists {
		if len(l) > 0 {
			return l
		}
	}
	return nil
}
