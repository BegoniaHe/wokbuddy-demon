// index.go 面板静态资源与安全响应头。
//
// 前端为独立构建产物（web/，Rsbuild + React），构建输出到 dist/ 后经 go:embed
// 打进二进制（随服务部署）。产物为带内容哈希的外部 JS/CSS，页面无内联脚本，
// 契合严格 CSP（script-src 'self'）。
//
// 安全头对"面板页面、静态资源与全部 /panel/api/* 响应"统一生效：CSP 限制脚本
// 只能来自本服务，禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露
// Referer 出去。
package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// panelFS 是去掉 dist/ 前缀后的资源文件系统（index.html + static/**）。
var panelFS = mustSubFS(distFS, "dist")

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("panel: 无法解析内嵌 dist 资源: " + err.Error())
	}

	return sub
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、图表），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面、资源与 API 都要）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// 不缓存：页面引用带哈希的资源，但 index.html 本身必须每次回源以拿到最新引用。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	raw, err := fs.ReadFile(panelFS, "index.html")
	if err != nil {
		http.Error(w, "panel: 缺少内嵌 index.html", http.StatusInternalServerError)
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// asset 输出构建产物（/panel/static/** 等）。
// 产物文件名带内容哈希，可长期强缓存（immutable）。
func (p *Panel) asset(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/panel/")
	if rel == "" || strings.HasPrefix(rel, "api/") {
		http.NotFound(w, r)
		return
	}

	if _, err := fs.Stat(panelFS, rel); err != nil {
		http.NotFound(w, r)
		return
	}

	setSecurityHeaders(w)

	if rel == "index.html" {
		// 直接访问 /panel/index.html 时也不可强缓存（须回源取最新资源引用）。
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		// 产物文件名带内容哈希，可长期强缓存（immutable）。
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	http.ServeFileFS(w, r, panelFS, rel)
}
