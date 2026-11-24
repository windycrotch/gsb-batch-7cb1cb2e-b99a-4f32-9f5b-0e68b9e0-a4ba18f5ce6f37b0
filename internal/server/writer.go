package server

import (
	"sync"

	"github.com/gorilla/websocket"
)

// connWriter 串行化一条 WebSocket 连接上的写操作。
// 会话收尾会在独立 goroutine 中向连接发送多条结果消息，而读循环也可能
// 发送文本消息；gorilla/websocket 不支持并发写，因此必须经过同一把锁。
type connWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func newConnWriter(conn *websocket.Conn) *connWriter {
	return &connWriter{conn: conn}
}

// WriteJSON 实现了 recording.Writer。
func (w *connWriter) WriteJSON(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(v)
}

// Close 实现了 recording.Writer。重复关闭是安全的（gorilla 返回错误即可忽略）。
func (w *connWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Close()
}
