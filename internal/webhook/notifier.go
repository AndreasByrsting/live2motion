// Package webhook 负责把任务事件推送到外部服务（钉钉/飞书/Slack/ntfy/Bark/自定义）。
//
// 设计要点：
//   - 同类事件在时间窗口内合并成一条，避免批量转换时刷屏；
//   - 按目标地址自动适配报文格式，用户不需要选平台；
//   - 推送失败只记日志、重试一次，绝不阻塞转换主流程。
package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/immich2motion/immich2motion/internal/config"
)

// Kind 事件类型。
type Kind string

const (
	KindStart   Kind = "task.start"
	KindSuccess Kind = "task.success"
	KindFailure Kind = "task.failure"
	KindSystem  Kind = "system"
	KindTest    Kind = "test" // 仅用于「发送测试消息」，不参与事件开关
)

// KindTitle 事件的中文标题（用于推送标题）。
func KindTitle(k Kind) string {
	switch k {
	case KindStart:
		return "开始转换"
	case KindSuccess:
		return "转换完成"
	case KindFailure:
		return "转换失败"
	case KindSystem:
		return "系统异常"
	case KindTest:
		return "测试消息"
	default:
		return string(k)
	}
}

// kindVerb 用于合并消息的动词。
func kindVerb(k Kind) string {
	switch k {
	case KindStart:
		return "开始处理"
	case KindSuccess:
		return "转换成功"
	case KindFailure:
		return "转换失败"
	case KindSystem:
		return "出现异常"
	case KindTest:
		return "测试"
	default:
		return "发生事件"
	}
}

// Event 一条待推送的事件。
type Event struct {
	Kind   Kind
	Title  string // 简短标识，如文件名
	Detail string // 详细描述
	At     time.Time
}

// Logger 日志回调，由 engine 注入。
type Logger func(level, format string, args ...any)

// maxBucket 单个合并桶的上限，达到后提前推送，避免长时间窗口堆积过多。
const maxBucket = 50

type bucket struct {
	events []Event
	timer  *time.Timer
}

// Notifier 事件聚合与推送。
type Notifier struct {
	mu      sync.Mutex
	cfg     config.Settings
	buckets map[Kind]*bucket
	client  *http.Client
	log     Logger
	closed  bool
}

// New 构造 Notifier。
func New(cfg config.Settings, log Logger) *Notifier {
	if log == nil {
		log = func(string, string, ...any) {}
	}
	return &Notifier{
		cfg:     cfg,
		buckets: make(map[Kind]*bucket),
		client:  &http.Client{Timeout: 12 * time.Second},
		log:     log,
	}
}

// Update 应用新配置；关键参数变化时清空待发队列，避免用旧地址发新消息。
func (n *Notifier) Update(cfg config.Settings) {
	n.mu.Lock()
	old := n.cfg
	n.cfg = cfg
	if old.WebhookURL != cfg.WebhookURL || !cfg.WebhookEnabled {
		for k, b := range n.buckets {
			if b.timer != nil {
				b.timer.Stop()
			}
			delete(n.buckets, k)
		}
	}
	n.mu.Unlock()
}

// Close 停止所有合并定时器，并把未发出的消息尝试发一次。
func (n *Notifier) Close() {
	n.mu.Lock()
	n.closed = true
	pending := make(map[Kind][]Event, len(n.buckets))
	cfg := n.cfg
	for k, b := range n.buckets {
		if b.timer != nil {
			b.timer.Stop()
		}
		if len(b.events) > 0 {
			pending[k] = b.events
		}
		delete(n.buckets, k)
	}
	n.mu.Unlock()

	for _, events := range pending {
		n.send(cfg, events)
	}
}

// Enabled 当前配置下该事件是否需要推送。
func (n *Notifier) Enabled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.WebhookEnabled && n.cfg.WebhookURL != ""
}

// allowed 判断该事件类型是否开启推送。
func allowed(cfg config.Settings, k Kind) bool {
	if !cfg.WebhookEnabled || cfg.WebhookURL == "" {
		return false
	}
	switch k {
	case KindStart:
		return cfg.WebhookOnStart
	case KindSuccess:
		return cfg.WebhookOnSuccess
	case KindFailure:
		return cfg.WebhookOnFailure
	case KindSystem:
		return cfg.WebhookOnSystem
	}
	return false
}

// Notify 提交一条事件。是否真正发送、是否合并由配置决定。
func (n *Notifier) Notify(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	cfg := n.cfg
	n.mu.Unlock()

	if !allowed(cfg, e.Kind) {
		return
	}

	// 窗口为 0：不合并，立即发
	if cfg.WebhookDigestSec <= 0 {
		go n.send(cfg, []Event{e})
		return
	}

	n.mu.Lock()
	b, ok := n.buckets[e.Kind]
	if !ok {
		b = &bucket{}
		n.buckets[e.Kind] = b
	}
	b.events = append(b.events, e)

	// 堆积过多时立即 flush，避免长窗口忘发
	if len(b.events) >= maxBucket {
		events := b.events
		if b.timer != nil {
			b.timer.Stop()
		}
		delete(n.buckets, e.Kind)
		n.mu.Unlock()
		go n.send(cfg, events)
		return
	}

	// 桶内第一条事件才启动定时器（固定窗口，从第一条开始计时）
	if b.timer == nil {
		kind := e.Kind
		window := time.Duration(cfg.WebhookDigestSec) * time.Second
		b.timer = time.AfterFunc(window, func() { n.flush(kind) })
	}
	n.mu.Unlock()
}

// flush 到期后取出整桶并推送。
func (n *Notifier) flush(kind Kind) {
	n.mu.Lock()
	b, ok := n.buckets[kind]
	if !ok || len(b.events) == 0 {
		n.mu.Unlock()
		return
	}
	events := b.events
	delete(n.buckets, kind)
	cfg := n.cfg
	closed := n.closed
	n.mu.Unlock()

	if closed {
		return
	}
	n.send(cfg, events)
}

// ---------- 报文构建 ----------

// buildText 把一组事件渲染成适合 IM 阅读的纯文本。
func buildText(cfg config.Settings, events []Event) (title, body string) {
	kind := events[0].Kind
	if len(events) == 1 {
		title = "[immich2motion] " + KindTitle(kind)
		e := events[0]
		body = e.Detail
		if e.Title != "" && !strings.Contains(e.Detail, e.Title) {
			body = e.Title + "：" + e.Detail
		}
		return title, body
	}

	title = fmt.Sprintf("[immich2motion] %d 个任务%s", len(events), kindVerb(kind))
	max := cfg.WebhookMaxItems
	if max < 1 {
		max = 10
	}
	lines := make([]string, 0, max+1)
	for i, e := range events {
		if i >= max {
			lines = append(lines, fmt.Sprintf("… 还有 %d 条", len(events)-max))
			break
		}
		lines = append(lines, "· "+e.Title)
	}
	body = strings.Join(lines, "\n")
	return title, body
}

// detectPlatform 根据 host 判断目标平台，用于选择报文格式。
func detectPlatform(u *url.URL) string {
	h := strings.ToLower(u.Host)
	switch {
	case strings.Contains(h, "oapi.dingtalk.com"):
		return "dingtalk"
	case strings.Contains(h, "open.feishu.cn"), strings.Contains(h, "open.larksuite.com"):
		return "feishu"
	case strings.Contains(h, "qyapi.weixin.qq.com"):
		return "wecom"
	case strings.Contains(h, "hooks.slack.com"):
		return "slack"
	case strings.Contains(h, "discord.com"), strings.Contains(h, "discordapp.com"):
		return "discord"
	case strings.Contains(h, "ntfy.sh"), strings.HasPrefix(h, "ntfy."):
		return "ntfy"
	case strings.Contains(h, "api.day.app"):
		return "bark"
	default:
		return "generic"
	}
}

// buildRequest 按平台生成请求体与额外 header。
func buildRequest(cfg config.Settings, events []Event) (body []byte, headers map[string]string, err error) {
	parsed, perr := url.Parse(cfg.WebhookURL)
	if perr != nil {
		return nil, nil, fmt.Errorf("解析 Webhook 地址失败: %w", perr)
	}
	title, content := buildText(cfg, events)
	text := title + "\n" + content
	platform := detectPlatform(parsed)

	headers = map[string]string{"Content-Type": "application/json; charset=utf-8"}

	switch platform {
	case "ntfy":
		// ntfy 用 JSON 发布，中文标题才不会被 HTTP header 的 ASCII 限制破坏
		topic := strings.Trim(parsed.Path, "/")
		if topic == "" {
			topic = "immich2motion"
		}
		b, _ := json.Marshal(map[string]any{
			"topic": topic, "title": title, "message": content,
			"tags": []string{"camera"},
		})
		return b, headers, nil

	case "bark":
		payload := map[string]any{"title": title, "body": content, "group": "immich2motion"}
		b, _ := json.Marshal(payload)
		return b, headers, nil

	case "slack":
		b, _ := json.Marshal(map[string]any{"text": text})
		return b, headers, nil

	case "discord":
		b, _ := json.Marshal(map[string]any{"content": text})
		return b, headers, nil

	case "dingtalk", "wecom":
		b, _ := json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
		return b, headers, nil

	case "feishu":
		b, _ := json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		})
		return b, headers, nil

	default:
		// 通用结构：便于自建接收端做结构化处理
		items := make([]string, 0, len(events))
		for _, e := range events {
			items = append(items, e.Title)
		}
		payload := map[string]any{
			"source":   "immich2motion",
			"event":    string(events[0].Kind),
			"title":    title,
			"message":  content,
			"count":    len(events),
			"items":    items,
			"at":       events[0].At.Format(time.RFC3339),
			"platform": "generic",
		}
		b, _ := json.Marshal(payload)
		return b, headers, nil
	}
}

// SendNow 立即发送一条事件，不经过合并窗口。用于「测试推送」按钮。
func (n *Notifier) SendNow(e Event) error {
	n.mu.Lock()
	cfg := n.cfg
	n.mu.Unlock()
	if !cfg.WebhookEnabled || cfg.WebhookURL == "" {
		return fmt.Errorf("Webhook 未启用或地址为空")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	body, headers, err := buildRequest(cfg, []Event{e})
	if err != nil {
		return err
	}
	return n.post(cfg.WebhookURL, body, headers)
}

// ---------- 发送 ----------

// send 同步发送，失败重试一次。所有错误只落日志。
func (n *Notifier) send(cfg config.Settings, events []Event) {
	if len(events) == 0 || cfg.WebhookURL == "" {
		return
	}
	body, headers, err := buildRequest(cfg, events)
	if err != nil {
		n.log("warn", "%v", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lastErr = n.post(cfg.WebhookURL, body, headers)
		if lastErr == nil {
			kind := events[0].Kind
			if len(events) > 1 {
				n.log("info", "Webhook 已推送：%s × %d 条", KindTitle(kind), len(events))
			} else {
				n.log("info", "Webhook 已推送：%s · %s", KindTitle(kind), events[0].Title)
			}
			return
		}
		if attempt == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	n.log("warn", "Webhook 推送失败（已重试 1 次）：%v", lastErr)
}

func (n *Notifier) post(endpoint string, body []byte, headers map[string]string) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "immich2motion/webhook")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 读掉响应体以便连接复用，同时截取前 300 字节用于报错
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, snippet)
	}
	// 部分平台（钉钉/飞书）HTTP 200 但业务码失败
	if errCode := detectBizError(respBody); errCode != "" {
		return fmt.Errorf("%s", errCode)
	}
	return nil
}

// detectBizError 识别钉钉/飞书的业务错误码（HTTP 200 但实际失败）。
func detectBizError(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if v, ok := m["errcode"]; ok {
		if code, ok := v.(float64); ok && code != 0 {
			return fmt.Sprintf("errcode=%v errmsg=%v", v, m["errmsg"])
		}
	}
	if v, ok := m["code"]; ok {
		if code, ok := v.(float64); ok && code != 0 {
			return fmt.Sprintf("code=%v msg=%v", v, m["msg"])
		}
	}
	return ""
}
