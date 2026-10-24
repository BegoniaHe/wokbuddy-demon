// chatter.go 任务链路短问句池：夜猫子与桌面体验任务出站 chat 的 user 内容。
//
// 固定问句（「1+1等于几？」）会让同一账号历史出现完全相同的 user 内容，
// 跨账号也共用同一句，形成内容层归并特征。这里改为每条对话从池中抽取。
//
// 数据来源：CommonsenseQA 2.0 dev 集（https://github.com/allenai/csqa2）。
// 过滤口径：validations 不含 sensitive / bad question、confidence >= 0.9、
// 问句长度 <= 80 字符，按问句文本去重，取按 id 排序后的前 150 条。
// 许可：CC BY 4.0；仓库仅内嵌问句文本，不随构建分发原始数据集。
package upstream

import (
	_ "embed"
	"math/rand/v2"
	"strings"
	"sync"
)

//go:embed chatter_questions.txt
var chatterQuestionData string

var chatterQuestions = strings.Split(strings.TrimSpace(chatterQuestionData), "\n")

var (
	chatterQuestionOffset = rand.IntN(len(chatterQuestions)) //nolint:gosec // 问句池随机起点，非安全用途
	chatterQuestionMu     sync.Mutex
	chatterQuestionCursor int
)

// pickChatQuestion 返回池中一条问句。随机起点 + 单调游标保证同进程内
// 连续两次调用不会命中同一条（并发调用同样成立），避免固定/重复问句。
func pickChatQuestion() string {
	n := len(chatterQuestions)

	chatterQuestionMu.Lock()
	chatterQuestionCursor = (chatterQuestionCursor + 1) % n
	idx := (chatterQuestionOffset + chatterQuestionCursor) % n
	chatterQuestionMu.Unlock()

	return chatterQuestions[idx]
}
