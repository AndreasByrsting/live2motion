package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/immich2motion/immich2motion/internal/config"
	"github.com/immich2motion/immich2motion/internal/store"
)

// ---------- 页面 ----------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.eng.Dashboard(r.Context())
	data := map[string]any{
		"Title": "仪表盘", "Active": "dashboard", "Version": config.Version, "D": d,
		"Health": d.Health,
	}
	if err != nil {
		data["FlashErr"] = "读取统计失败: " + err.Error()
	}
	s.render(w, "dashboard", data)
}

func (s *Server) handleJobsPage(w http.ResponseWriter, r *http.Request) {
	data := s.jobsData(r)
	data["Title"] = "任务列表"
	data["Active"] = "jobs"
	data["Version"] = config.Version
	data["Health"] = s.eng.Health()
	s.render(w, "jobs", data)
}

func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	limit := 300
	logs, err := s.st.ListLogs(r.Context(), level, limit)
	data := map[string]any{
		"Title": "日志", "Active": "logs", "Version": config.Version,
		"Logs": logs, "Level": level, "Health": s.eng.Health(),
	}
	if err != nil {
		data["FlashErr"] = "读取日志失败: " + err.Error()
	}
	s.render(w, "logs", data)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	st := s.eng.Settings()
	s.render(w, "settings", map[string]any{
		"Title": "设置", "Active": "settings", "Version": config.Version,
		"Settings": st, "Health": s.eng.Health(),
		"Flash":    r.URL.Query().Get("ok"),
		"FlashErr": r.URL.Query().Get("err"),
	})
}

// jobsData 组装任务列表页与局部刷新共用的数据。
func (s *Server) jobsData(r *http.Request) map[string]any {
	q := r.URL.Query()
	status := q.Get("status")
	query := q.Get("q")
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	const limit = 50
	jobs, total, err := s.st.ListJobs(r.Context(), store.JobFilter{
		Status: status, Query: query, Limit: limit, Offset: (page - 1) * limit,
	})
	pages := int((total + limit - 1) / limit)
	if pages < 1 {
		pages = 1
	}
	data := map[string]any{
		"Jobs": jobs, "JobTotal": total, "Page": page, "Pages": pages,
		"Status": status, "Query": query,
	}
	if err != nil {
		data["FlashErr"] = "读取任务失败: " + err.Error()
	}
	return data
}

// ---------- 局部片段 ----------

func (s *Server) handleStatsPartial(w http.ResponseWriter, r *http.Request) {
	d, err := s.eng.Dashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "stats", map[string]any{"D": d})
}

func (s *Server) handleDashboardBodyPartial(w http.ResponseWriter, r *http.Request) {
	d, err := s.eng.Dashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "dashboard_body", map[string]any{"D": d})
}

func (s *Server) handleJobsPartial(w http.ResponseWriter, r *http.Request) {
	data := s.jobsData(r)
	s.renderPartial(w, "jobs_table", data)
}

func (s *Server) handleLogsPartial(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	logs, err := s.st.ListLogs(r.Context(), level, 300)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "log_rows", map[string]any{"Logs": logs})
}

// ---------- 操作 ----------

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if !s.eng.TriggerScan("manual") {
		s.toast(w, "已有扫描在进行中", true)
		return
	}
	s.toast(w, "已触发扫描，稍后刷新查看结果", false)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.eng.SetPaused(true)
	s.toast(w, "任务已暂停", false)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.eng.SetPaused(false)
	s.toast(w, "任务已恢复", false)
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	// 切换 dry-run；开启时立刻触发一次扫描，方便先看配对结果
	next := !s.eng.DryRun()
	s.eng.SetDryRun(next)
	if next {
		s.eng.TriggerScan("dry-run")
		s.toast(w, "Dry-Run 已开启：仅扫描配对，不执行转换", false)
		return
	}
	s.toast(w, "Dry-Run 已关闭", false)
}

func (s *Server) handleResetFailed(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.ResetFailed(r.Context(), false)
	if err != nil {
		s.toast(w, "清除失败标记失败: "+err.Error(), true)
		return
	}
	s.toast(w, fmt.Sprintf("已清除 %d 条失败标记", n), false)
	s.eng.Wake()
}

func (s *Server) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.ResetFailed(r.Context(), true)
	if err != nil {
		s.toast(w, "重置失败任务出错: "+err.Error(), true)
		return
	}
	s.toast(w, fmt.Sprintf("已重新排队 %d 个失败任务", n), false)
	s.eng.Wake()
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	pairID, err := strconv.ParseInt(r.FormValue("pair_id"), 10, 64)
	if err != nil || pairID <= 0 {
		s.toast(w, "参数错误", true)
		return
	}
	if err := s.st.ResetJobByPair(r.Context(), pairID); err != nil {
		s.toast(w, "重新排队失败: "+err.Error(), true)
		return
	}
	s.eng.Wake()
	s.toast(w, "已重新排队该任务", false)
}

func (s *Server) handleDeepVerify(w http.ResponseWriter, r *http.Request) {
	if !s.eng.TriggerDeepVerify() {
		s.toast(w, "已有扫描在进行中，请稍后再试", true)
		return
	}
	s.toast(w, "已触发深度 MD5 校验，完成后在日志中查看结果", false)
}

func (s *Server) handleRecheck(w http.ResponseWriter, r *http.Request) {
	rep := s.eng.Recheck(r.Context())
	// 返回整个横幅片段，htmx 直接替换，用户能立刻看到每项检查结果
	s.renderPartial(w, "health_banner", map[string]any{"Health": rep})
}

func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.SendTestWebhook(); err != nil {
		s.toast(w, "测试推送失败: "+err.Error(), true)
		return
	}
	s.toast(w, "测试消息已发送，请检查接收端（使用的是已保存的配置）", false)
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if err := s.st.ClearLogs(r.Context()); err != nil {
		s.toast(w, "清空日志失败: "+err.Error(), true)
		return
	}
	s.toast(w, "日志已清空", false)
}

// ---------- 设置 ----------

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.toast(w, "表单解析失败", true)
		return
	}
	cur := s.eng.Settings()
	next := cur

	next.SourceDir = strings.TrimSpace(r.FormValue("source_dir"))
	next.OutputDir = strings.TrimSpace(r.FormValue("output_dir"))
	next.ScanIntervalSec = atoiOr(r.FormValue("scan_interval_sec"), cur.ScanIntervalSec)
	next.Workers = atoiOr(r.FormValue("workers"), cur.Workers)
	next.JPGQuality = atoiOr(r.FormValue("jpg_quality"), cur.JPGQuality)
	next.MaxRetries = atoiOr(r.FormValue("max_retries"), cur.MaxRetries)
	next.DeepVerifyHours = atoiOr(r.FormValue("deep_verify_hours"), cur.DeepVerifyHours)
	next.LogRetention = atoiOr(r.FormValue("log_retention"), cur.LogRetention)
	next.OutputSuffix = strings.TrimSpace(r.FormValue("output_suffix"))
	next.EnableMD5 = r.FormValue("enable_md5") != ""
	next.AutoScan = r.FormValue("auto_scan") != ""
	next.Paused = r.FormValue("paused") != ""
	next.ReconvertOnChange = r.FormValue("reconvert_on_change") != ""
	next.PreserveOrientation = r.FormValue("preserve_orientation") != ""
	next.VerifyBeforeConvert = r.FormValue("verify_before_convert") != ""
	next.IncludeUnconverted = r.FormValue("include_unconverted") != ""
	if r.FormValue("mirror_settle_min") != "" {
		next.MirrorSettleMin = atoiOr(r.FormValue("mirror_settle_min"), cur.MirrorSettleMin)
	}
	next.AutoTheme = r.FormValue("auto_theme") != ""
	if v := strings.TrimSpace(r.FormValue("heic_exts")); v != "" {
		next.HeicExts = splitExts(v)
	}
	if v := strings.TrimSpace(r.FormValue("video_exts")); v != "" {
		next.VideoExts = splitExts(v)
	}

	// Webhook 通知
	next.WebhookURL = strings.TrimSpace(r.FormValue("webhook_url"))
	next.WebhookEnabled = r.FormValue("webhook_enabled") != ""
	next.WebhookOnStart = r.FormValue("webhook_on_start") != ""
	next.WebhookOnSuccess = r.FormValue("webhook_on_success") != ""
	next.WebhookOnFailure = r.FormValue("webhook_on_failure") != ""
	next.WebhookOnSystem = r.FormValue("webhook_on_system") != ""
	next.WebhookMaxItems = atoiOr(r.FormValue("webhook_max_items"), cur.WebhookMaxItems)

	if err := s.eng.UpdateSettings(r.Context(), next); err != nil {
		s.toast(w, "保存失败: "+err.Error(), true)
		return
	}
	s.toast(w, "设置已保存并立即生效", false)
}

func atoiOr(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func splitExts(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// toast 返回一条提示片段，htmx 直接换入页面右上角容器。
func (s *Server) toast(w http.ResponseWriter, msg string, isErr bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cls := "toast toast-ok"
	if isErr {
		cls = "toast toast-err"
	}
	fmt.Fprintf(w, `<div class="%s">%s</div>`, cls, html.EscapeString(msg))
}

// ---------- JSON / 健康检查 ----------

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	d, err := s.eng.Dashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(d)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := s.st.DB().PingContext(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"error","db":"down"}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
