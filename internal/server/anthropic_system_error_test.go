package server

import (
	"encoding/json/jsontext"
	"errors"
	"testing"
)

// TestAnthropicSystemContentKeepsParseError 畸形 system 块：通用哨兵仍可 errors.Is
// 命中，且底层解析原因不被丢弃——排障时只有「解析失败」一个信息无从定位畸形形态。
func TestAnthropicSystemContentKeepsParseError(t *testing.T) {
	t.Parallel()

	_, err := anthropicSystemContent(jsontext.Value(`[{"type":"text",`))
	if err == nil {
		t.Fatal("畸形 system 块应报错")
	}

	if !errors.Is(err, errAnthropicMessageFailed) {
		t.Fatalf("err=%v 应可由 errors.Is 命中原哨兵", err)
	}

	// 改动前这里只有哨兵文案本身（底层解析错误被丢弃），故该断言能抓回退。
	sentinelOnly := "invalid system content: " + errAnthropicMessageFailed.Error()
	if err.Error() == sentinelOnly {
		t.Fatalf("err=%q 丢失底层解析原因", err.Error())
	}
}
