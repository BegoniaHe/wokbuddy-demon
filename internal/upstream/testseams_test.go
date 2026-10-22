package upstream

import (
	"io"
	"net/http"
)

// 测试便利入口：只覆盖生产入口的默认参数组合，避免每个用例重复写 nil。
// 生产代码一律走 PrepareBodyOptWithEffortsAndDefault / StreamHint。

func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}
