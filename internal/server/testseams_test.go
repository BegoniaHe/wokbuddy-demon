package server

import "net/http"

// 测试便利入口：只覆盖生产入口的默认参数组合，避免每个用例重复写零值。
// 生产代码一律走带 Options/Client/Namespaces 的完整入口。

func anthropicToOpenAI(body []byte) ([]byte, string, bool, error) {
	converted, err := anthropicConvert(body)
	if err != nil {
		return nil, "", false, err
	}

	return converted.openAI, converted.model, converted.stream, nil
}

// anthropicConvert 解码入站请求体并转换，等价于生产入口
// anthropicConvertWithSearch 的「不执行搜索」分支。
func anthropicConvert(body []byte) (anthropicConversion, error) {
	req, err := decodeAnthropicRequest(body)
	if err != nil {
		return anthropicConversion{}, err
	}

	return anthropicConvertRequest(req)
}

func anthropicMessageFromOpenAI(body any, model string) map[string]any {
	return anthropicMessageFromOpenAIWithOptions(body, model, anthropicResponseOptions{})
}

func newAnthropicStreamWriter(w http.ResponseWriter, model string) *anthropicStreamWriter {
	return newAnthropicStreamWriterWithOptions(w, model, anthropicResponseOptions{})
}

func responsesToOpenAI(body []byte) ([]byte, string, string, bool, error) {
	return responsesToOpenAIWithClient(body, nil, nil)
}

func responsesFromOpenAI(body any, model, instructions string, customTools ...map[string]bool) map[string]any {
	custom := map[string]bool{}
	if len(customTools) > 0 {
		custom = customTools[0]
	}

	return responsesFromOpenAIWithNamespaces(body, model, instructions, custom, nil)
}
