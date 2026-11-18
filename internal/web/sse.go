package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/immich2motion/immich2motion/internal/engine"
)

// handleSSE 把后台日志、任务进度、统计变化实时推给浏览器。
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式响应", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	subID, ch := s.eng.Bus().Subscribe()
	defer s.eng.Bus().Unsubscribe(subID)

	// 首帧下发快照，页面无需额外请求即可渲染当前状态
	if d, err := s.eng.Dashboard(r.Context()); err == nil {
		writeSSE(w, flusher, engine.EventSnapshot, d)
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, ev.Type, ev.Data)
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return
	}
	f.Flush()
}
