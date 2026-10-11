// Package bus 进程内事件总线：模块只管发事件，API/SSE 层订阅后推给浏览器。
package bus

import (
	"sync"
)

// Event 一条事件：Topic 用于扇出（status/traffic/devices/system/logs/ap/relay/upstream…），Data 为任意 JSON 可序列化载荷
type Event struct {
	Topic string
	Data  any
}

// Bus 发布/订阅，多 topic 订阅去重
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]map[string]bool // 订阅者 -> 它关心的 topic 集合
}

func New() *Bus {
	return &Bus{subs: make(map[chan Event]map[string]bool)}
}

// Subscribe 订阅一组 topic；返回通道与取消函数。缓冲 256，慢消费者由 SSE 层丢弃旧事件。
func (b *Bus) Subscribe(topics ...string) (ch chan Event, cancel func()) {
	ch = make(chan Event, 256)
	set := make(map[string]bool, len(topics))
	for _, t := range topics {
		set[t] = true
	}
	b.mu.Lock()
	b.subs[ch] = set
	b.mu.Unlock()
	cancel = func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return ch, cancel
}

// Publish 向关心该 topic 的订阅者非阻塞投递（满了就丢，实时数据宁可新弃旧）
func (b *Bus) Publish(topic string, data any) {
	ev := Event{Topic: topic, Data: data}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, set := range b.subs {
		if !set[topic] {
			continue
		}
		select {
		case ch <- ev:
		default: // 丢弃积压
		}
	}
}
