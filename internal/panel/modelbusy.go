// modelbusy.go 面板模型页的官方繁忙度观测（issue #258）。
//
// 纯只读：失败只并入模型页 fetchErrs，不阻断模型目录、不参与选号。
package panel

import (
	"context"
	"strings"
	"time"
)

// attachModelBusy 给已构造的模型条目补 busy 字段（按 realm 各取一个可用账号）。
//
//nolint:gocognit // 逐模型附加繁忙度并汇总错误，线性展开。
func (p *Panel) attachModelBusy(out []map[string]any, fetchErrs *[]string) {
	for _, realm := range []string{"cn", "global"} {
		names := modelNamesForRealm(out, realm)
		if len(names) == 0 {
			continue
		}

		uids := p.cfg.Pool.AvailableUIDsForRealm(realm)
		if len(uids) == 0 {
			continue
		}

		account := p.cfg.Pool.AuthByUID(uids[0])
		if account == nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		levels, err := p.cfg.Upstream.ModelBusy(ctx, account, names)

		cancel()

		if err != nil {
			*fetchErrs = append(*fetchErrs, realm+": busy: "+err.Error())

			continue
		}

		for _, entry := range out {
			id, _ := entry["id"].(string)

			prefix := realm + ":"
			if !strings.HasPrefix(id, prefix) {
				continue
			}

			if level, ok := levels[strings.TrimPrefix(id, prefix)]; ok {
				entry["busy"] = level
			}
		}
	}
}

// modelNamesForRealm 提取某 realm 的裸模型名（去重）。
func modelNamesForRealm(out []map[string]any, realm string) []string {
	prefix := realm + ":"
	seen := map[string]bool{}
	names := make([]string, 0, len(out))

	for _, entry := range out {
		id, _ := entry["id"].(string)
		if !strings.HasPrefix(id, prefix) {
			continue
		}

		name := strings.TrimPrefix(id, prefix)
		if name == "" || seen[name] {
			continue
		}

		seen[name] = true
		names = append(names, name)
	}

	return names
}
