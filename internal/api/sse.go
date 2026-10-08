// sse.go —— Server-Sent Events：订阅内部事件总线并扇出给浏览器。
// 一个端点 GET /api/events?topics=status,traffic,devices,system
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 所有可订阅 topic（未指定时全订）
var allTopics = []string{"status", "traffic", "devices", "system", "logs", "upstream", "relay"}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "该连接不支持流式响应")
		return
	}
	topics := allTopics
	if q := r.URL.Query().Get("topics"); q != "" {
		topics = splitComma(q)
	}
	ch, cancel := s.evbus.Subscribe(topics...)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, ": connected\n\n")
	fmt.Fprintf(w, "event: hello\ndata: {\"topics\":%q}\n\n", topics)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, data)
			flusher.Flush()
		}
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
