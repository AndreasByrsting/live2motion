// Package web 提供内嵌的管理面板（服务端渲染 + htmx 局部刷新 + SSE 实时推送）。
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/engine"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

//go:embed templates assets
var content embed.FS

// Server Web 服务。
type Server struct {
	cfg     config.Config
	st      *store.Store
	eng     *engine.Engine
	pages   map[string]*template.Template
	partial *template.Template
}

// NewServer 加载模板并构造服务。
func NewServer(cfg config.Config, st *store.Store, eng *engine.Engine) (*Server, error) {
	s := &Server{cfg: cfg, st: st, eng: eng, pages: map[string]*template.Template{}}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

var funcMap = template.FuncMap{
	"fmtBytes":    fmtBytes,
	"fmtTime":     fmtTime,
	"fmtAgo":      fmtAgo,
	"fmtDuration": fmtDuration,
	"pct":         func(v float64) string { return strconv.Itoa(int(v*100 + 0.5)) },
	"scanPct": func(s engine.ScanStatus) int {
		if s.Total <= 0 {
			return 0
		}
		v := int(float64(s.Done) * 100 / float64(s.Total))
		if v > 100 {
			v = 100
		}
		return v
	},
	"scanPhaseText": scanPhaseText,
	"statusText":    statusText,
	"statusClass":   statusClass,
	"stageText":     stageText,
	"truncate":      truncate,
	"dec":           func(n int) int { return n - 1 },
	"inc":           func(n int) int { return n + 1 },
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
}

func (s *Server) loadTemplates() error {
	for _, name := range []string{"dashboard", "jobs", "logs", "settings"} {
		t, err := template.New("layout").Funcs(funcMap).
			ParseFS(content, "templates/layout.html", "templates/"+name+".html", "templates/partials/*.html")
		if err != nil {
			return fmt.Errorf("解析模板 %s 失败: %w", name, err)
		}
		s.pages[name] = t
	}
	pt, err := template.New("partials").Funcs(funcMap).
		ParseFS(content, "templates/partials/*.html")
	if err != nil {
		return fmt.Errorf("解析局部模板失败: %w", err)
	}
	s.partial = pt
	return nil
}

// Routes 注册所有路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	staticFS, err := fs.Sub(content, "assets")
	if err != nil {
		panic(err)
	}
	staticServer := http.StripPrefix("/static/", cacheControl(http.FileServerFS(staticFS)))
	mux.Handle("GET /static/", staticServer)

	// 页面
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /jobs", s.handleJobsPage)
	mux.HandleFunc("GET /logs", s.handleLogsPage)
	mux.HandleFunc("GET /settings", s.handleSettingsPage)

	// 局部片段（htmx）
	mux.HandleFunc("GET /partials/stats", s.handleStatsPartial)
	mux.HandleFunc("GET /partials/jobs", s.handleJobsPartial)
	mux.HandleFunc("GET /partials/logs", s.handleLogsPartial)
	mux.HandleFunc("GET /partials/dashboard-body", s.handleDashboardBodyPartial)

	// 实时事件
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /api/status", s.handleStatusJSON)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// 操作
	mux.HandleFunc("POST /api/actions/scan", s.handleScan)
	mux.HandleFunc("POST /api/actions/pause", s.handlePause)
	mux.HandleFunc("POST /api/actions/resume", s.handleResume)
	mux.HandleFunc("POST /api/actions/dry-run", s.handleDryRun)
	mux.HandleFunc("POST /api/actions/reset-failed", s.handleResetFailed)
	mux.HandleFunc("POST /api/actions/retry-failed", s.handleRetryFailed)
	mux.HandleFunc("POST /api/actions/retry", s.handleRetryJob)
	mux.HandleFunc("POST /api/actions/deep-verify", s.handleDeepVerify)
	mux.HandleFunc("POST /api/actions/recheck", s.handleRecheck)
	mux.HandleFunc("POST /api/actions/webhook-test", s.handleWebhookTest)
	mux.HandleFunc("POST /api/actions/clear-logs", s.handleClearLogs)

	// 设置
	mux.HandleFunc("POST /api/settings", s.handleSaveSettings)

	return logRequests(mux)
}

// cacheControl 用协商缓存（no-cache + Last-Modified）。
// 静态文件名不带 hash，若用长缓存，升级后浏览器会继续用旧 CSS/JS，界面看起来没变化。
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源与 SSE 不记录，避免噪音
		if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/api/events" {
			_ = r
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- 模板辅助 ----------

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "页面不存在", http.StatusNotFound)
		return
	}
	// 布局里要用到「是否自动切换配色」与版本号，统一在这里注入，
	// 免得每个 handler 都记得塞一遍。
	if m, ok := data.(map[string]any); ok {
		m["AutoTheme"] = s.eng.Settings().AutoTheme
		if _, has := m["Version"]; !has {
			m["Version"] = config.Version
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "渲染失败: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partial.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "渲染片段失败: "+err.Error(), http.StatusInternalServerError)
	}
}

// ---------- 格式化 ----------

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("01-02 15:04:05")
}

func fmtAgo(ts int64) string {
	if ts <= 0 {
		return "从未"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}

func fmtDuration(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

func statusText(s string) string {
	switch s {
	case store.JobPending:
		return "待处理"
	case store.JobRunning:
		return "处理中"
	case store.JobSuccess:
		return "成功"
	case store.JobFailed:
		return "失败"
	case store.JobSkipped:
		return "已跳过"
	default:
		return s
	}
}

func statusClass(s string) string {
	switch s {
	case store.JobPending:
		return "badge badge-pending"
	case store.JobRunning:
		return "badge badge-running"
	case store.JobSuccess:
		return "badge badge-success"
	case store.JobFailed:
		return "badge badge-failed"
	case store.JobSkipped:
		return "badge badge-skip"
	default:
		return "badge"
	}
}

func stageText(s string) string {
	switch s {
	case "queued":
		return "排队中"
	case "prepare":
		return "准备"
	case "heic->jpg":
		return "HEIC→JPG"
	case "xmp":
		return "写入 XMP"
	case "concat":
		return "拼接视频"
	case "verify":
		return "校验"
	case "done":
		return "完成"
	case "source-changed":
		return "源文件变化"
	case "recovered":
		return "崩溃恢复"
	case "dry-run":
		return "Dry-Run"
	default:
		return s
	}
}

// scanPhaseText 扫描阶段的中文说明，让用户知道「卡」在哪一步。
func scanPhaseText(p string) string {
	switch p {
	case "walk":
		return "遍历目录、登记文件"
	case "content_id":
		return "读取 ContentIdentifier"
	case "md5":
		return "计算 MD5 校验值"
	case "pair":
		return "配对并生成任务"
	default:
		return "准备扫描"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
