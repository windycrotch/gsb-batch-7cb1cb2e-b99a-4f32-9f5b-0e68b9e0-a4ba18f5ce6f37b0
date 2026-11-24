// Package recording 管理单条 WebSocket 连接上的录音会话：音频缓冲、
// 会话结束时的收尾（存储 + 识别）以及结果消息的发送。
//
// 设计目标：
//   - 每个会话的结束只处理一次；重复的结束信号不会重复上传或重复识别。
//   - 存储结果与识别结果都会发送给客户端，且顺序固定（存储结果先于识别结果），
//     便于客户端和测试观察完整的收尾过程。
//   - 存储或识别失败时发送明确的错误消息，而不是静默无响应。
//   - 一次性会话只有在结果消息全部发出后才关闭连接。
//   - 会话结束后立即释放音频缓冲，之后该会话的音频数据会被拒绝。
//   - 多个会话的缓冲彼此独立，结束一个会话不会影响其它仍在录音的会话。
package recording

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/telepace/voiceflow/internal/storage"
	"github.com/telepace/voiceflow/internal/stt"
)

var (
	// ErrSessionNotFound 表示连接上不存在该会话。
	ErrSessionNotFound = errors.New("recording: session not found")
	// ErrSessionEnded 表示会话已经结束（或已被清理），不再接受音频数据。
	ErrSessionEnded = errors.New("recording: session already ended")
	// ErrSessionExists 表示同一会话 ID 仍处于录音状态。
	ErrSessionExists = errors.New("recording: session already active")
	// ErrEmptySessionID 表示会话 ID 为空。
	ErrEmptySessionID = errors.New("recording: empty session id")
)

// Writer 是收尾结果消息的发送目标。*websocket.Conn 本身满足该接口；
// 测试中可以注入一个记录消息的假实现。
type Writer interface {
	WriteJSON(v interface{}) error
	Close() error
}

// Outcome 描述一次会话收尾的最终结果，便于调用方记录日志或测试断言。
type Outcome struct {
	SessionID    string
	OneShot      bool
	AudioURL     string
	StorageErr   error
	Text         string
	RecognizeErr error
	// WriteErr 记录发送结果消息时出现的错误（例如连接已断开）。
	WriteErr error
}

// session 保存单个会话的缓冲和状态。会话结束后 buffer 置为 nil 以释放内存，
// 同时 ended 置为 true 作为墓碑，防止同一 ID 被重复收尾。
type session struct {
	buffer  *bytes.Buffer
	oneShot bool
	ended   bool
}

// Manager 管理一条连接上的多个录音会话。它可以被多个 goroutine 并发使用：
// 每个连接的 WebSocket 读循环和各个会话的收尾 goroutine 会同时访问它。
type Manager struct {
	sttSvc     stt.Service
	storageSvc storage.Service

	mu       sync.Mutex
	sessions map[string]*session
	// order 按开始顺序保存仍在录音的会话 ID；Current 返回最后一个。
	order []string
	// pendingFinalizes 记录已经 BeginEnd、尚未完成 Finalize 的会话数。
	pendingFinalizes int
	// closePending 表示有一次性会话要求关闭连接。只有在该连接上既没有
	// 仍在录音的会话、也没有未完成的收尾时才会真正关闭，避免摧毁其它
	// 会话尚未送达的数据。
	closePending bool
	// closeWriter 是发起一次性关闭请求的写入器。延迟关闭时关闭它，
	// 而不是碰巧触发收尾的另一个写入器。
	closeWriter Writer
}

// NewManager 创建一个会话管理器。sttSvc/storageSvc 可以为 nil，
// 对应的收尾步骤会被当作失败并回报给客户端。
func NewManager(sttSvc stt.Service, storageSvc storage.Service) *Manager {
	return &Manager{
		sttSvc:     sttSvc,
		storageSvc: storageSvc,
		sessions:   make(map[string]*session),
	}
}

// Start 开始一个新的录音会话。同一个 ID 不能重复开始：
// 活跃会话返回 ErrSessionExists，已结束的会话返回 ErrSessionEnded。
func (m *Manager) Start(sessionID string, oneShot bool) error {
	if sessionID == "" {
		return ErrEmptySessionID
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[sessionID]; ok {
		if s.ended {
			return fmt.Errorf("%w: %s", ErrSessionEnded, sessionID)
		}
		return fmt.Errorf("%w: %s", ErrSessionExists, sessionID)
	}

	m.sessions[sessionID] = &session{buffer: &bytes.Buffer{}, oneShot: oneShot}
	m.order = append(m.order, sessionID)
	return nil
}

// SetOneShot 在会话进行中更新其「一次性」标记。会话已结束或不存在时返回错误。
func (m *Manager) SetOneShot(sessionID string, oneShot bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if s.ended {
		return fmt.Errorf("%w: %s", ErrSessionEnded, sessionID)
	}
	s.oneShot = oneShot
	return nil
}

// Current 返回最近开始且仍在录音的会话 ID，没有则返回空字符串。
func (m *Manager) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.order) == 0 {
		return ""
	}
	return m.order[len(m.order)-1]
}

// ActiveSessions 返回仍在录音的会话 ID（按开始顺序）。主要用于观测和测试。
func (m *Manager) ActiveSessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// Append 向指定会话追加音频数据。会话不存在或已结束时返回错误，
// 不会写入已释放的缓冲。
func (m *Manager) Append(sessionID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if s.ended || s.buffer == nil {
		return fmt.Errorf("%w: %s", ErrSessionEnded, sessionID)
	}

	_, err := s.buffer.Write(data)
	return err
}

// AppendCurrent 向当前（最近开始且仍在录音）会话追加音频数据，返回接收
// 数据的会话 ID。
//
// 二进制音频帧本身不携带会话 ID（现有客户端协议如此），因此只能按「当前
// 会话」归属。调用方若需要严格的多会话并发录音，应使用 Append 并显式指定
// 会话 ID。
func (m *Manager) AppendCurrent(data []byte) (string, error) {
	m.mu.Lock()
	if len(m.order) == 0 {
		m.mu.Unlock()
		return "", ErrSessionNotFound
	}
	sessionID := m.order[len(m.order)-1]
	s := m.sessions[sessionID]
	if s == nil || s.ended || s.buffer == nil {
		m.mu.Unlock()
		return sessionID, fmt.Errorf("%w: %s", ErrSessionEnded, sessionID)
	}
	_, err := s.buffer.Write(data)
	m.mu.Unlock()
	return sessionID, err
}

// PendingEnd 是一次已经「摘取」但尚未完成收尾的会话。
//
// BeginEnd 会在调用方 goroutine 中同步地把会话标记为已结束并摘下音频数据，
// 因此之后到达的音频数据会立即被拒绝，重复的结束信号也会立即被拒绝；
// 耗时的存储与识别则由 Finalize 在后台完成。
type PendingEnd struct {
	manager   *Manager
	sessionID string
	audioData []byte
	oneShot   bool
}

// SessionID 返回该待收尾会话的 ID。
func (p *PendingEnd) SessionID() string { return p.sessionID }

// OneShot 返回该会话是否为一次性会话。
func (p *PendingEnd) OneShot() bool { return p.oneShot }

// BeginEnd 同步地结束会话并摘下音频数据（缓冲立即释放）。
//
// 对不存在或已结束的会话返回错误，且不会产生任何副作用，从而保证重复的
// 结束信号不会重复上传或重复识别。返回的 PendingEnd 由调用方决定何时
// 执行 Finalize。
func (m *Manager) BeginEnd(sessionID string) (*PendingEnd, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if s.ended || s.buffer == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionEnded, sessionID)
	}

	// 先标记结束再释放锁，保证并发的重复结束信号只有一个能继续执行。
	s.ended = true
	audioData := make([]byte, s.buffer.Len())
	copy(audioData, s.buffer.Bytes())
	s.buffer = nil // 释放该会话占用的音频缓冲
	oneShot := s.oneShot
	m.removeFromOrderLocked(sessionID)
	// 记录一次未完成的收尾，避免一次性会话在其它会话收尾完成前关闭连接。
	m.pendingFinalizes++

	return &PendingEnd{
		manager:   m,
		sessionID: sessionID,
		audioData: audioData,
		oneShot:   oneShot,
	}, nil
}

// Finalize 并发执行音频存储与语音识别，把结果送达连接，并在收尾完成后
// 视情况关闭一次性会话的连接。
//
// 一次性会话的「关闭连接」会被延后：只有当该连接上既没有仍在录音的会话、
// 也没有其它尚未完成的收尾时才会真正关闭，从而不会摧毁同一连接上其它会话
// 尚未送达的数据。若调用方要求更强的隔离，应让一次性会话独占连接。
func (p *PendingEnd) Finalize(w Writer) *Outcome {
	outcome := p.manager.finalize(p.sessionID, p.audioData, p.oneShot, w)
	p.manager.finishFinalize(p.oneShot, w)
	return outcome
}

// End 是 BeginEnd + Finalize 的同步组合，便于不需要异步收尾的调用方使用。
//
// 对已结束的会话再次调用 End 会返回 ErrSessionEnded，且不会再次上传或识别。
// 返回的 Outcome 用于调用方记录日志，存储/识别本身的失败已通过消息通知客户端。
func (m *Manager) End(sessionID string, w Writer) (*Outcome, error) {
	pending, err := m.BeginEnd(sessionID)
	if err != nil {
		return nil, err
	}
	return pending.Finalize(w), nil
}

// finalize 执行存储与识别，并把结果按固定顺序送达连接。
//
// 存储结果一就绪就立即发送，识别结果随后发送，因此慢识别不会阻塞音频地址
// 的交付；固定顺序（存储结果先于识别结果）仍然得到保证。
func (m *Manager) finalize(sessionID string, audioData []byte, oneShot bool, w Writer) *Outcome {
	outcome := &Outcome{SessionID: sessionID, OneShot: oneShot}

	// 存储与识别互不依赖，并发执行。
	type storageResult struct {
		url string
		err error
	}
	type recognitionResult struct {
		text string
		err  error
	}
	storageCh := make(chan storageResult, 1)
	recognitionCh := make(chan recognitionResult, 1)

	go func() {
		if m.storageSvc == nil {
			storageCh <- storageResult{err: errors.New("storage service unavailable")}
			return
		}
		url, err := m.storageSvc.StoreAudio(audioData)
		storageCh <- storageResult{url: url, err: err}
	}()
	go func() {
		if m.sttSvc == nil {
			recognitionCh <- recognitionResult{err: errors.New("stt service unavailable")}
			return
		}
		text, err := m.sttSvc.Recognize(audioData, "")
		recognitionCh <- recognitionResult{text: text, err: err}
	}()

	// 先发存储结果：一旦存储返回就交付，不必等识别完成。
	sr := <-storageCh
	outcome.AudioURL, outcome.StorageErr = sr.url, sr.err
	if outcome.WriteErr = m.sendStorageResult(w, outcome); outcome.WriteErr != nil {
		<-recognitionCh // 回收识别 goroutine，避免泄漏
		return outcome
	}

	// 再发识别结果。
	rr := <-recognitionCh
	outcome.Text, outcome.RecognizeErr = rr.text, rr.err
	if outcome.WriteErr = m.sendRecognitionResult(w, outcome); outcome.WriteErr != nil {
		return outcome
	}

	outcome.WriteErr = w.WriteJSON(map[string]interface{}{
		"type":       "session_end",
		"session_id": sessionID,
		"one_shot":   oneShot,
	})
	return outcome
}

// finishFinalize 在一次收尾结束后递减计数，并在所有条件满足时关闭连接。
//
// 关闭被推迟到「没有仍在录音的会话、也没有尚未完成的收尾」时才执行，
// 因此一次性的会话不会摧毁同一连接上其它会话尚未送达的数据。
func (m *Manager) finishFinalize(oneShot bool, w Writer) {
	m.mu.Lock()
	if m.pendingFinalizes > 0 {
		m.pendingFinalizes--
	}
	if oneShot {
		m.closePending = true
		m.closeWriter = w
	}
	shouldClose := m.closePending && m.pendingFinalizes == 0 && len(m.order) == 0
	var toClose Writer
	if shouldClose {
		toClose = m.closeWriter
		m.closePending = false
		m.closeWriter = nil
	}
	m.mu.Unlock()

	if toClose != nil {
		// 所有结果消息均已写出，此时关闭是安全的。
		toClose.Close()
	}
}

func (m *Manager) sendStorageResult(w Writer, outcome *Outcome) error {
	if outcome.StorageErr != nil {
		return w.WriteJSON(map[string]interface{}{
			"type":       "storage_error",
			"session_id": outcome.SessionID,
			"error":      outcome.StorageErr.Error(),
		})
	}
	return w.WriteJSON(map[string]interface{}{
		"type":       "audio_stored",
		"session_id": outcome.SessionID,
		"audio_url":  outcome.AudioURL,
	})
}

func (m *Manager) sendRecognitionResult(w Writer, outcome *Outcome) error {
	if outcome.RecognizeErr != nil {
		return w.WriteJSON(map[string]interface{}{
			"type":       "recognition_error",
			"session_id": outcome.SessionID,
			"error":      outcome.RecognizeErr.Error(),
		})
	}
	return w.WriteJSON(map[string]interface{}{
		"type":       "recognition_complete",
		"session_id": outcome.SessionID,
		"text":       outcome.Text,
	})
}

// Cleanup 释放会话资源但不做收尾（例如连接断开）。之后该会话的音频会被拒绝。
func (m *Manager) Cleanup(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return
	}
	s.buffer = nil
	s.ended = true
	m.removeFromOrderLocked(sessionID)
}

// ReleaseAll 释放连接上所有会话的音频缓冲，用于连接关闭时避免内存泄漏。
func (m *Manager) ReleaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range m.sessions {
		s.buffer = nil
		s.ended = true
	}
	m.order = nil
	m.closePending = false
	m.closeWriter = nil
}

// removeFromOrderLocked 从活跃会话顺序中移除指定会话，调用方需持有锁。
func (m *Manager) removeFromOrderLocked(sessionID string) {
	for i, id := range m.order {
		if id == sessionID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}
