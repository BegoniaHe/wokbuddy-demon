package upstream

import (
	"encoding/json"
	"io"
	"net/http"
)

// 测试便利入口：只覆盖生产入口的默认参数组合，避免每个用例重复写 nil。
// 生产代码一律走 rewriteChatBody（chat 出站单轮改写）/ StreamHint。

// PrepareBodyOptWithEffortsAndDefault 基础管线入口（efforts 降级 + thinking.go 按
// defaultEfforts 补档，historyKeepTurns > 0 时按轮裁剪历史）。production 把它作为
// rewriteChatBody 的零值形态内联调用；这里保留给「只做基础规范化」的用例。
func PrepareBodyOptWithEffortsAndDefault(
	src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, historyKeepTurns int,
) []byte {
	return rewriteChatBody(src, &chatBodyRewrite{
		sanitize: sanitize, efforts: efforts, defaultEfforts: defaultEfforts, historyKeepTurns: historyKeepTurns,
	})
}

func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil, 0)
}

func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil, 0)
}

// injectPromptCacheKey 对整份 body 套用 prompt_cache_key 注入（cache key 单测入口）。
// 生产链路上这一步在 rewriteChatBody 的同一次解码里执行，不再单独整解整编。
func injectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body // 坏 body / 顶层 null：无字段可写，保持原文
	}

	injectPromptCacheKeyObj(obj, uid, conversationID)

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}

	return out
}

func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}
