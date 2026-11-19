// Package webhook 负责把任务事件推送到外部服务（钉钉/飞书/Slack/ntfy/Bark/自定义）。
//
// 设计要点：
//   - 一条事件 = 一条推送。合并窗口已移除：调用方（engine）现在以
//     「一轮扫描 → 队列排空」为单位汇总好内容，逐任务推送不会再发生；
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
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
		return "开始处理"
	case KindSuccess:
		return "处理完成"
	case KindFailure:
		return "处理完成（有失败）"
	case KindSystem:
		return "系统异常"
	case KindTest:
		return "测试消息"
	default:
		return string(k)
	}
}

// Event 一条待推送的事件。
type Event struct {
	Kind   Kind
	Title  string // 推送标题（为空时用 KindTitle）
	Detail string // 正文，支持多行
	At     time.Time
}

// Logger 日志回调，由 engine 注入。
type Logger func(level, format string, args ...any)

// Notifier 推送器。
type Notifier struct {
	mu     sync.Mutex
	cfg    config.Settings
	client *http.Client
	log    Logger
	closed bool
}

// New 构造 Notifier。
func New(cfg config.Settings, log Logger) *Notifier {
	if log == nil {
		log = func(string, string, ...any) {}
	}
	return &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 12 * time.Second},
		log:    log,
	}
}

// Update 应用新配置。
func (n *Notifier) Update(cfg config.Settings) {
	n.mu.Lock()
	n.cfg = cfg
	n.mu.Unlock()
}

// Close 标记关闭：之后提交的事件不再发送。已发出的请求无法中断，无需等待。
func (n *Notifier) Close() {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
}

// Enabled 当前配置下是否有可用的推送通道。
func (n *Notifier) Enabled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg.WebhookEnabled && n.cfg.WebhookURL != ""
}

// Allowed 判断某类事件在当前配置下是否需要推送。
func Allowed(cfg config.Settings, k Kind) bool {
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

// Notify 提交一条事件。是否真正发送由配置决定，发送是异步的。
func (n *Notifier) Notify(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	n.mu.Lock()
	cfg := n.cfg
	closed := n.closed
	n.mu.Unlock()
	if closed || !Allowed(cfg, e.Kind) {
		return
	}
	go n.send(cfg, e)
}

// ---------- 报文构建 ----------

// buildText 把一条事件渲染成适合 IM 阅读的标题与正文。
func buildText(e Event) (title, body string) {
	title = "[live2motion] " + KindTitle(e.Kind)
	if strings.TrimSpace(e.Title) != "" {
		title = "[live2motion] " + strings.TrimSpace(e.Title)
	}
	body = strings.TrimSpace(e.Detail)
	if body == "" {
		body = KindTitle(e.Kind)
	}
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
	case strings.Contains(h, "api.day.app"), strings.Contains(h, "bark."):
		return "bark"
	default:
		return "generic"
	}
}

// buildRequest 按平台生成端点、请求体与额外 header。
//
// 返回的 endpoint 通常等于配置里的地址，只有 Bark 例外：它的路径式地址会被服务器
// 当作标题/正文解析，必须改写端点才能用 JSON 提交。
func buildRequest(cfg config.Settings, e Event) (endpoint string, body []byte, headers map[string]string, err error) {
	parsed, perr := url.Parse(cfg.WebhookURL)
	if perr != nil {
		return "", nil, nil, fmt.Errorf("解析 Webhook 地址失败: %w", perr)
	}
	title, content := buildText(e)
	text := title + "\n" + content
	platform := detectPlatform(parsed)
	endpoint = parsed.String()

	headers = map[string]string{"Content-Type": "application/json; charset=utf-8"}

	switch platform {
	case "ntfy":
		// ntfy 用 JSON 发布，中文标题才不会被 HTTP header 的 ASCII 限制破坏
		topic := strings.Trim(parsed.Path, "/")
		if topic == "" {
			topic = "live2motion"
		}
		b, _ := json.Marshal(map[string]any{
			"topic": topic, "title": title, "message": content,
			"tags": []string{"camera"},
		})
		return endpoint, b, headers, nil

	case "bark":
		ep, b, err := barkRequest(parsed, title, content)
		return ep, b, headers, err

	case "slack":
		b, _ := json.Marshal(map[string]any{"text": text})
		return endpoint, b, headers, nil

	case "discord":
		b, _ := json.Marshal(map[string]any{"content": text})
		return endpoint, b, headers, nil

	case "dingtalk", "wecom":
		b, _ := json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
		return endpoint, b, headers, nil

	case "feishu":
		b, _ := json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		})
		return endpoint, b, headers, nil

	default:
		// 通用结构：便于自建接收端做结构化处理
		payload := map[string]any{
			"source":   "live2motion",
			"event":    string(e.Kind),
			"title":    title,
			"message":  content,
			"at":       e.At.Format(time.RFC3339),
			"platform": "generic",
		}
		b, _ := json.Marshal(payload)
		return endpoint, b, headers, nil
	}
}

// barkRequest 生成 Bark 推送。
//
// Bark 的地址有两种形态：
//   - 只带设备 key：https://api.day.app/<key>
//   - App 里直接复制的路径式地址：https://api.day.app/<key>/<标题>/<正文>
//
// 后者用 POST 时服务器只认路径里的标题与正文、丢弃 JSON 请求体，于是通知里
// 显示的就是路径上的字面量（例如用户照抄模板时的 body）。所以这里只保留
// 第一段作为设备 key，改写成 POST /<key>，标题正文走 JSON；地址上附带的
// 参数（group / ttl / sound / level / icon 等）原样搬进请求体，保持用户在
// App 里选好的推送行为。
func barkRequest(parsed *url.URL, title, body string) (string, []byte, error) {
	key := firstPathSegment(parsed.Path)
	if key == "" {
		return "", nil, fmt.Errorf("Bark 地址缺少设备 key，应形如 https://api.day.app/<key>")
	}

	ep := *parsed
	ep.Path = "/" + key
	ep.RawQuery = ""
	ep.Fragment = ""
	ep.RawPath = ""

	payload := map[string]any{"title": title, "body": body}
	for k, vs := range parsed.Query() {
		if len(vs) == 0 {
			continue
		}
		switch k {
		case "title", "body", "device_key", "device_keys":
			continue // 由程序决定，避免和实际内容冲突
		}
		payload[k] = barkValue(vs[0])
	}
	if _, ok := payload["group"]; !ok {
		payload["group"] = "live2motion"
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("构造 Bark 报文失败: %w", err)
	}
	return ep.String(), b, nil
}

func firstPathSegment(p string) string {
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg = strings.TrimSpace(seg); seg != "" {
			return seg
		}
	}
	return ""
}

// barkValue 把地址参数的字符串转成合适的 JSON 类型：Bark 的 ttl / badge 之类
// 字段是数字，传字符串会被判为参数错误。
func barkValue(v string) any {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}

// SendNow 立即发送一条事件。用于「测试推送」按钮。
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
	endpoint, body, headers, err := buildRequest(cfg, e)
	if err != nil {
		return err
	}
	return n.post(endpoint, body, headers)
}

// ---------- 发送 ----------

// send 同步发送，失败重试一次。所有错误只落日志。
func (n *Notifier) send(cfg config.Settings, e Event) {
	if cfg.WebhookURL == "" {
		return
	}
	endpoint, body, headers, err := buildRequest(cfg, e)
	if err != nil {
		n.log("warn", "%v", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lastErr = n.post(endpoint, body, headers)
		if lastErr == nil {
			n.log("info", "Webhook 已推送：%s", buildTitleForLog(e))
			return
		}
		if attempt == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	n.log("warn", "Webhook 推送失败（已重试 1 次）：%v", lastErr)
}

func buildTitleForLog(e Event) string {
	if strings.TrimSpace(e.Title) != "" {
		return e.Title
	}
	return KindTitle(e.Kind)
}

func (n *Notifier) post(endpoint string, body []byte, headers map[string]string) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "live2motion/webhook")

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
	// 部分平台（钉钉/飞书）HTTP 200 但业务码失败。注意不能对所有平台都查 code：
	// Bark 成功时返回 {"code":200,...}，按「code≠0 即失败」会误判成失败并重试。
	if errCode := detectBizError(platformOf(endpoint), respBody); errCode != "" {
		return fmt.Errorf("%s", errCode)
	}
	return nil
}

func platformOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return detectPlatform(u)
	}
	return "generic"
}

// detectBizError 识别钉钉/飞书/企微的业务错误码（HTTP 200 但实际失败）。
func detectBizError(platform string, body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	switch platform {
	case "dingtalk", "wecom":
		if v, ok := m["errcode"]; ok {
			if code, ok := v.(float64); ok && code != 0 {
				return fmt.Sprintf("errcode=%v errmsg=%v", v, m["errmsg"])
			}
		}
	case "feishu":
		if v, ok := m["code"]; ok {
			if code, ok := v.(float64); ok && code != 0 {
				return fmt.Sprintf("code=%v msg=%v", code, m["msg"])
			}
		}
	}
	return ""
}
