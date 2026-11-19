package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/AndreasByrsting/live2motion/internal/config"
)

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, b)
	}
	return m
}

// Bark App 里复制的地址是路径式的 /<key>/<标题>/<正文>。POST 到这种地址时
// 服务器只认路径，JSON 请求体被丢弃，通知里显示的就是路径上的字面量。
// 这里验证端点被改写成 /<key>，标题正文走 JSON。
func TestBarkPathStyleURL(t *testing.T) {
	cfg := config.Settings{
		WebhookEnabled: true,
		WebhookURL:     "https://api.day.app/BiCFcdc7GaJGwveU2wnm3M/live2motion/body?group=immich2motion&ttl=600",
	}
	ep, body, _, err := buildRequest(cfg, Event{Kind: KindSuccess, Detail: "本轮转换 3 张，耗时 1m2s"})
	if err != nil {
		t.Fatalf("buildRequest 失败: %v", err)
	}
	if ep != "https://api.day.app/BiCFcdc7GaJGwveU2wnm3M" {
		t.Fatalf("端点应只保留设备 key，实际 %s", ep)
	}
	m := decodeBody(t, body)
	if got := m["body"]; got != "本轮转换 3 张，耗时 1m2s" {
		t.Fatalf("body 应为实际内容，实际 %v", got)
	}
	if got := m["title"]; got != "[live2motion] 处理完成" {
		t.Fatalf("title 异常: %v", got)
	}
	// 地址上的参数要原样带过去，否则用户设置的 ttl/分组会失效
	if got := m["group"]; got != "immich2motion" {
		t.Fatalf("group 应保留地址里的值，实际 %v", got)
	}
	if got, ok := m["ttl"].(float64); !ok || got != 600 {
		t.Fatalf("ttl 应为数字 600，实际 %#v", m["ttl"])
	}
}

func TestBarkKeyOnlyURLDefaultsGroup(t *testing.T) {
	cfg := config.Settings{
		WebhookEnabled: true,
		WebhookURL:     "https://api.day.app/ABC123/",
	}
	ep, body, _, err := buildRequest(cfg, Event{Kind: KindStart, Detail: "开始扫描图库"})
	if err != nil {
		t.Fatalf("buildRequest 失败: %v", err)
	}
	if ep != "https://api.day.app/ABC123" {
		t.Fatalf("端点异常: %s", ep)
	}
	m := decodeBody(t, body)
	if m["group"] != "live2motion" {
		t.Fatalf("未指定分组时应默认 live2motion，实际 %v", m["group"])
	}
	if _, ok := m["ttl"]; ok {
		t.Fatalf("未指定 ttl 时不应凭空写入: %v", m["ttl"])
	}
}

func TestBarkMissingKey(t *testing.T) {
	cfg := config.Settings{WebhookEnabled: true, WebhookURL: "https://api.day.app/"}
	if _, _, _, err := buildRequest(cfg, Event{Kind: KindSystem, Detail: "x"}); err == nil {
		t.Fatal("缺少设备 key 时应报错")
	}
}

func TestBarkSelfHosted(t *testing.T) {
	u, _ := url.Parse("https://bark.example.com/KEY/t/body")
	if got := detectPlatform(u); got != "bark" {
		t.Fatalf("自建 Bark 域名应识别为 bark，实际 %s", got)
	}
}

// 非 Bark 地址不应被改写端点。
func TestOtherPlatformsKeepEndpoint(t *testing.T) {
	cfg := config.Settings{
		WebhookEnabled: true,
		WebhookURL:     "https://ntfy.sh/my-topic",
	}
	ep, body, _, err := buildRequest(cfg, Event{Kind: KindSuccess, Detail: "ok"})
	if err != nil {
		t.Fatalf("buildRequest 失败: %v", err)
	}
	if ep != cfg.WebhookURL {
		t.Fatalf("ntfy 端点不应被改写，实际 %s", ep)
	}
	m := decodeBody(t, body)
	if m["topic"] != "my-topic" {
		t.Fatalf("topic 异常: %v", m["topic"])
	}
}

// rewriteTransport 把请求改投到测试服务器，便于验证真实发送流程。
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// 端到端：SendNow 走完构造报文 → 发请求，最终落在 /<key> 上且正文正确。
func TestSendNowBarkPostsToKeyEndpoint(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	target, _ := url.Parse(srv.URL)
	n := New(config.Settings{
		WebhookEnabled: true,
		WebhookURL:     "https://api.day.app/ABC/old-title/body?ttl=600",
	}, nil)
	n.client = &http.Client{Transport: rewriteTransport{target: target}}

	if err := n.SendNow(Event{Kind: KindTest, Detail: "测试正文"}); err != nil {
		t.Fatalf("SendNow 失败: %v", err)
	}
	if gotPath != "/ABC" {
		t.Fatalf("请求应发到 /ABC，实际 %s", gotPath)
	}
	m := decodeBody(t, []byte(gotBody))
	if m["body"] != "测试正文" {
		t.Fatalf("正文异常: %v", m["body"])
	}
}
