// desktop_import.go 从本机 WorkBuddy 桌面客户端只读导入登录态（issue #98）。
//
// 客户端 auth 文件的 token 可能是 $wbEncrypted 字段（AES-256-GCM）。密钥由用户
// 在面板一次性输入；本文件不落盘、不写日志、不在响应里回显 token。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/desktopcred"
)

// desktopImportMaxBody 导入请求体上限；key 与路径列表都很小。
const desktopImportMaxBody = 64 << 10

// desktopScan 扫描本机桌面客户端凭据目录，只返回元数据（不含 token）。
func (p *Panel) desktopScan(w http.ResponseWriter, _ *http.Request) {
	accounts := desktopcred.Scan()
	if accounts == nil {
		accounts = []desktopcred.Account{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": accounts})
}

// desktopImport 用一次性 at-rest secret 解密并导入选中的凭据文件。
// 请求：{"key":"<base64>","paths":["..."]}；paths 为空 = 扫描到的全部文件。
func (p *Panel) desktopImport(w http.ResponseWriter, r *http.Request) {
	req, err := parseDesktopImport(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	discovered := desktopcred.Discover()
	allowed := make(map[string]bool, len(discovered))
	for _, path := range discovered {
		allowed[path] = true
	}
	targets := req.Paths
	if len(targets) == 0 {
		targets = discovered
	}
	if len(targets) > 100 {
		writeErr(w, http.StatusBadRequest, "too many credential files")
		return
	}
	imported, errs, importedAuths := p.importDesktopFiles(targets, allowed, req.Key)
	p.postImportAccounts(importedAuths)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    len(targets),
		"imported": imported,
		"skipped":  len(targets) - imported,
		"errors":   errs,
	})
}

func parseDesktopImport(r *http.Request) (struct {
	Key   string   `json:"key"`
	Paths []string `json:"paths"`
}, error,
) {
	var req struct {
		Key   string   `json:"key"`
		Paths []string `json:"paths"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, desktopImportMaxBody+1))
	if err != nil {
		return req, fmt.Errorf("read body: %w", err)
	}
	if len(raw) > desktopImportMaxBody {
		return req, errors.New("request body too large")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("invalid json: %w", err)
	}
	req.Key = strings.TrimSpace(req.Key)
	if len(req.Key) > 1024 {
		return req, errors.New("at-rest secret too long")
	}
	return req, nil
}

// importDesktopFiles 逐文件导入；只允许发现列表内的路径，避免任意文件读取。
func (p *Panel) importDesktopFiles(targets []string, allowed map[string]bool, key string) (int, []string, []*auth.Auth) {
	var imported int
	errs := make([]string, 0, len(targets))
	importedAuths := make([]*auth.Auth, 0, len(targets))
	for _, path := range targets {
		base := filepath.Base(path)
		if !allowed[path] {
			errs = append(errs, base+": not a discovered credential file")
			continue
		}
		a, err := desktopcred.ImportFile(path, key)
		if err != nil {
			errs = append(errs, base+": "+err.Error())
			continue
		}
		uid := strings.TrimSpace(a.UID)
		if !validImportUID(uid) {
			errs = append(errs, base+": invalid uid")
			continue
		}
		a.FilePath = filepath.Join(p.cfg.AuthDir, "workbuddy-"+uid+".json")
		if err := p.persistImportedAuth(a, a.Realm(), uid); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		imported++
		importedAuths = append(importedAuths, a)
	}
	return imported, errs, importedAuths
}
