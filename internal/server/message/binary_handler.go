package message

import (
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/telepace/voiceflow/internal/server/recording"
	"github.com/telepace/voiceflow/internal/storage"
	"github.com/telepace/voiceflow/internal/stt"
	"github.com/telepace/voiceflow/internal/tts"
)

// BinaryMessageHandler 处理一条连接上的录音会话。音频缓冲、会话级 one_shot
// 标记和收尾流程由 recording.Manager 统一管理，因此多个会话互不影响。
type BinaryMessageHandler struct {
	mu      sync.Mutex
	manager *recording.Manager
	writers map[*websocket.Conn]recording.Writer
}

func NewBinaryMessageHandler(sttSvc stt.Service, ttsSvc tts.Service, storageSvc storage.Service) *BinaryMessageHandler {
	// ttsSvc 由文本消息处理器使用；此处保留参数以维持既有构造签名。
	_ = ttsSvc
	return &BinaryMessageHandler{
		manager: recording.NewManager(sttSvc, storageSvc),
		writers: make(map[*websocket.Conn]recording.Writer),
	}
}

// HandleStart 处理音频开始信号。one_shot 按会话保存，不影响其它会话。
func (h *BinaryMessageHandler) HandleStart(sessionID string, oneShot bool) error {
	if err := h.manager.Start(sessionID, oneShot); err != nil {
		return fmt.Errorf("failed to start session: %w", err)
	}
	return nil
}

// HandleAudioData 处理音频二进制数据。会话结束后的数据会被拒绝。
func (h *BinaryMessageHandler) HandleAudioData(sessionID string, data []byte) error {
	if err := h.manager.Append(sessionID, data); err != nil {
		return fmt.Errorf("failed to write audio data: %w", err)
	}
	return nil
}

// HandleEnd 处理音频结束信号：完成存储与识别并回传结果，一次性会话在
// 结果消息发出后才关闭连接。重复的结束信号返回错误且不会重复处理。
func (h *BinaryMessageHandler) HandleEnd(sessionID string, conn *websocket.Conn) error {
	return h.handleEnd(sessionID, h.writerFor(conn))
}

// handleEnd 是 HandleEnd 与 recording.Writer 的组合，便于在不建立真实
// WebSocket 连接的情况下复用收尾流程。
func (h *BinaryMessageHandler) handleEnd(sessionID string, w recording.Writer) error {
	_, err := h.manager.End(sessionID, w)
	return err
}

// CleanupSession 清理指定会话的资源。
func (h *BinaryMessageHandler) CleanupSession(sessionID string) {
	h.manager.Cleanup(sessionID)
}

// ReleaseConnection 释放与一条连接相关的会话和写入器，应在连接关闭时调用，
// 避免 writers map 随连接数无界增长。
func (h *BinaryMessageHandler) ReleaseConnection(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.writers, conn)
	h.mu.Unlock()
}

func (h *BinaryMessageHandler) writerFor(conn *websocket.Conn) recording.Writer {
	h.mu.Lock()
	defer h.mu.Unlock()

	if w, ok := h.writers[conn]; ok {
		return w
	}
	w := &serialWriter{conn: conn}
	h.writers[conn] = w
	return w
}

// serialWriter 串行化同一连接上的写操作，满足 recording.Writer 接口。
type serialWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *serialWriter) WriteJSON(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteJSON(v)
}

func (w *serialWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Close()
}
