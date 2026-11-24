package server

import (
	"sync"

	"github.com/gorilla/websocket"
	"github.com/telepace/voiceflow/internal/server/recording"
	"github.com/telepace/voiceflow/pkg/logger"
)

// SessionManager 是 recording.Manager 的兼容封装，供历史调用方使用。
//
// 收尾（存储 + 识别 + 结果回传 + 一次性会话关闭）统一由 recording.Manager
// 完成，保证行为与 WebSocket 处理器一致。新代码应直接使用 recording.Manager。
type SessionManager struct {
	manager *recording.Manager

	writersMu sync.Mutex
	// writers 按连接缓存写入器，保证同一连接上的写操作串行。
	writers map[*websocket.Conn]*connWriter
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		manager: recording.NewManager(sttService, storageService),
		writers: make(map[*websocket.Conn]*connWriter),
	}
}

// StartSession 开始一个录音会话。
func (sm *SessionManager) StartSession(sessionID string) {
	if err := sm.manager.Start(sessionID, false); err != nil {
		logger.Error("开始录音会话失败", "session_id", sessionID, "error", err)
	}
}

// GetCurrentSession 返回最近开始的活跃会话 ID。
func (sm *SessionManager) GetCurrentSession() string {
	return sm.manager.Current()
}

// AppendAudioData 向指定会话追加音频数据。会话已结束时返回错误。
func (sm *SessionManager) AppendAudioData(sessionID string, data []byte) error {
	return sm.manager.Append(sessionID, data)
}

// EndSession 异步完成会话收尾。摘取会话同步执行，因此重复调用不会重复
// 上传或重复识别；耗时的存储、识别与结果回传在后台进行。
//
// 返回值表示结束信号是否被接受：会话不存在或已经结束时返回错误，与旧实现
// 一致，便于调用方发现无效的 audio_end。
func (sm *SessionManager) EndSession(sessionID string, ws *websocket.Conn) error {
	writer := sm.writerFor(ws)
	pending, err := sm.manager.BeginEnd(sessionID)
	if err != nil {
		return err
	}
	go pending.Finalize(writer)
	return nil
}

// CleanupSession 释放会话缓冲。
func (sm *SessionManager) CleanupSession(sessionID string) {
	sm.manager.Cleanup(sessionID)
}

// ReleaseConnection 释放与连接相关的写入器，应在连接关闭时调用。
func (sm *SessionManager) ReleaseConnection(ws *websocket.Conn) {
	sm.writersMu.Lock()
	delete(sm.writers, ws)
	sm.writersMu.Unlock()
	sm.manager.ReleaseAll()
}

func (sm *SessionManager) writerFor(ws *websocket.Conn) *connWriter {
	sm.writersMu.Lock()
	defer sm.writersMu.Unlock()

	if w, ok := sm.writers[ws]; ok {
		return w
	}
	w := newConnWriter(ws)
	sm.writers[ws] = w
	return w
}
