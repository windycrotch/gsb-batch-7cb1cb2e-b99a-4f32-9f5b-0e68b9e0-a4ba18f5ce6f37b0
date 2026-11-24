package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStorage / testSTT 是用于端到端测试的假实现，不依赖任何云服务。
type testStorage struct {
	mu   sync.Mutex
	got  [][]byte
	err  error
	wait time.Duration
	n    int
}

func (s *testStorage) StoreAudio(audioData []byte) (string, error) {
	if s.wait > 0 {
		time.Sleep(s.wait)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.got = append(s.got, append([]byte(nil), audioData...))
	if s.err != nil {
		return "", s.err
	}
	return "https://fake.storage/audio/" + strconv.Itoa(s.n) + ".wav", nil
}

func (s *testStorage) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *testStorage) audio(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[i]
}

func (s *testStorage) snapshotAudio() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.got))
	copy(out, s.got)
	return out
}

type testSTT struct {
	mu   sync.Mutex
	err  error
	text string
	wait time.Duration
	n    int
}

func (s *testSTT) Recognize(audioData []byte, audioURL string) (string, error) {
	if s.wait > 0 {
		time.Sleep(s.wait)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.err != nil {
		return "", s.err
	}
	if s.text != "" {
		return s.text, nil
	}
	return "text:" + string(audioData), nil
}

func (s *testSTT) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// startTestServer 启动一个带假服务的 WebSocket 服务端，并在测试结束时恢复全局服务。
func startTestServer(t *testing.T, sttSvc *testSTT, storageSvc *testStorage) *httptest.Server {
	t.Helper()

	prevSTT, prevStorage := sttService, storageService
	sttService, storageService = sttSvc, storageSvc
	t.Cleanup(func() {
		sttService, storageService = prevSTT, prevStorage
	})

	srv := NewServer()
	mux := http.NewServeMux()
	srv.SetupRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendJSON(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	require.NoError(t, conn.WriteJSON(v))
}

// inbox 在后台持续读取连接上的消息。这样测试可以按时间窗口断言「没有新消息」，
// 而不必在同一连接上使用读超时（gorilla 的连接在读超时后不可继续使用）。
type inbox struct {
	messages chan map[string]interface{}
	closed   chan struct{}
}

func newInbox(conn *websocket.Conn) *inbox {
	in := &inbox{
		messages: make(chan map[string]interface{}, 32),
		closed:   make(chan struct{}),
	}
	go func() {
		defer close(in.closed)
		for {
			var msg map[string]interface{}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			in.messages <- msg
		}
	}()
	return in
}

// next 等待下一条消息；超时返回 false。
func (in *inbox) next(t *testing.T, d time.Duration) (map[string]interface{}, bool) {
	t.Helper()
	select {
	case msg := <-in.messages:
		return msg, true
	case <-time.After(d):
		return nil, false
	}
}

// expectTypes 依次校验接下来收到的消息类型。
func (in *inbox) expectTypes(t *testing.T, d time.Duration, want ...string) []map[string]interface{} {
	t.Helper()
	got := make([]map[string]interface{}, 0, len(want))
	for _, wantType := range want {
		msg, ok := in.next(t, d)
		require.True(t, ok, "等待消息 %q 超时", wantType)
		actual, _ := msg["type"].(string)
		require.Equal(t, wantType, actual)
		got = append(got, msg)
	}
	return got
}

// expectNone 断言在时间窗口内没有新消息。
func (in *inbox) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	if msg, ok := in.next(t, d); ok {
		t.Fatalf("不应收到额外消息，但收到了: %v", msg)
	}
}

// expectClosed 断言服务端已关闭连接。
func (in *inbox) expectClosed(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-in.closed:
	case <-time.After(d):
		t.Fatal("连接未被关闭")
	}
}

func findByType(messages []map[string]interface{}, msgType string) map[string]interface{} {
	for _, m := range messages {
		if m["type"] == msgType {
			return m
		}
	}
	return nil
}

const recvTimeout = 5 * time.Second

func TestWebSocketRecordingOneShotDeliversResultsBeforeClose(t *testing.T) {
	stt := &testSTT{text: "你好世界"}
	storage := &testStorage{}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_start", "session_id": "s1", "one_shot": true,
	})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("chunk-1")))
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("chunk-2")))
	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_end", "session_id": "s1", "one_shot": true,
	})

	messages := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	in.expectClosed(t, recvTimeout)

	stored := findByType(messages, "audio_stored")
	assert.Equal(t, "s1", stored["session_id"])
	assert.Equal(t, "https://fake.storage/audio/1.wav", stored["audio_url"])

	recognized := findByType(messages, "recognition_complete")
	assert.Equal(t, "你好世界", recognized["text"])

	assert.Equal(t, 1, storage.calls())
	assert.Equal(t, []byte("chunk-1chunk-2"), storage.audio(0))
	assert.Equal(t, 1, stt.calls())
}

func TestWebSocketRecordingDuplicateEndIgnored(t *testing.T) {
	stt := &testSTT{}
	storage := &testStorage{wait: 30 * time.Millisecond}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "dup"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("audio")))
	// 连续发送两次结束信号，模拟客户端重复提交。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "dup"})
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "dup"})

	in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	in.expectNone(t, 300*time.Millisecond)

	assert.Equal(t, 1, storage.calls(), "重复结束不得重复上传")
	assert.Equal(t, 1, stt.calls(), "重复结束不得重复识别")
}

func TestWebSocketRecordingStorageFailureNotifiesClient(t *testing.T) {
	stt := &testSTT{text: "partial"}
	storage := &testStorage{err: errors.New("对象存储不可用")}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "s1"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("audio")))
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "s1"})

	messages := in.expectTypes(t, recvTimeout, "storage_error", "recognition_complete", "session_end")

	storageErr := findByType(messages, "storage_error")
	require.NotNil(t, storageErr)
	assert.Equal(t, "s1", storageErr["session_id"])
	assert.Contains(t, storageErr["error"], "对象存储不可用")
}

func TestWebSocketRecordingRecognitionFailureNotifiesClient(t *testing.T) {
	stt := &testSTT{err: errors.New("识别服务超时")}
	storage := &testStorage{}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_start", "session_id": "s1", "one_shot": true,
	})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("audio")))
	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_end", "session_id": "s1", "one_shot": true,
	})

	messages := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_error", "session_end")
	in.expectClosed(t, recvTimeout)

	recognizeErr := findByType(messages, "recognition_error")
	require.NotNil(t, recognizeErr)
	assert.Contains(t, recognizeErr["error"], "识别服务超时")
}

func TestWebSocketRecordingInterleavedSessionsIsolated(t *testing.T) {
	stt := &testSTT{}
	storage := &testStorage{wait: 20 * time.Millisecond}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	// 会话 A 开始录音，写入部分音频。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "A"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("A1")))

	// 会话 B 在同一连接上开始，写入自己的音频。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "B"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("B1")))

	// 结束 A：不能清掉仍在录音的 B 的数据。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "A"})
	first := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	assert.Equal(t, "A", findByType(first, "audio_stored")["session_id"])

	// B 继续录音并结束（一次性会话）。
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("B2")))
	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_end", "session_id": "B", "one_shot": true,
	})

	second := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	assert.Equal(t, "B", findByType(second, "audio_stored")["session_id"])
	in.expectClosed(t, recvTimeout)

	// A 的音频只包含自己的数据，B 不受 A 结束的影响。
	// A 在 B 结束之前已经完成存储，因此这里按内容而不是调用序号断言，
	// 以免依赖两个会话收尾 goroutine 的调度顺序。
	assert.Equal(t, 2, storage.calls())
	assert.ElementsMatch(t, [][]byte{[]byte("A1"), []byte("B1B2")}, storage.snapshotAudio())
}

func TestWebSocketRecordingOneShotDoesNotKillOtherSession(t *testing.T) {
	stt := &testSTT{}
	storage := &testStorage{wait: 20 * time.Millisecond}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	// 同一连接上：A 长时间录音，B 是一次性会话。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "A"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("A1")))

	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "B", "one_shot": true})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("B1")))

	// 结束一次性的 B。此时 A 仍在录音，连接不能被关闭。
	sendJSON(t, conn, map[string]interface{}{
		"type": "audio_end", "session_id": "B", "one_shot": true,
	})
	bResults := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	assert.Equal(t, "B", findByType(bResults, "audio_stored")["session_id"])

	// 连接仍然可用：A 继续录音并结束，其数据与结果必须完整送达。
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("A2")))
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "A"})

	aResults := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	assert.Equal(t, "A", findByType(aResults, "audio_stored")["session_id"])
	in.expectClosed(t, recvTimeout)

	assert.Equal(t, 2, storage.calls())
	assert.ElementsMatch(t, [][]byte{[]byte("A1A2"), []byte("B1")}, storage.snapshotAudio())
}

func TestWebSocketRecordingRejectsAudioAfterSessionEnd(t *testing.T) {
	stt := &testSTT{}
	storage := &testStorage{}
	ts := startTestServer(t, stt, storage)
	conn := dialWS(t, ts)
	in := newInbox(conn)

	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "s1"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("audio")))
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "s1"})
	in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")

	// 会话已结束，再发音频不应影响已上传的内容，也不应触发新的处理。
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("late")))
	in.expectNone(t, 200*time.Millisecond)
	assert.Equal(t, 1, storage.calls())
	assert.Equal(t, []byte("audio"), storage.audio(0))

	// 连接仍然可用：可以开始一个新的会话，旧会话的缓冲不会串到新会话。
	sendJSON(t, conn, map[string]interface{}{"type": "audio_start", "session_id": "s2"})
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("second")))
	sendJSON(t, conn, map[string]interface{}{"type": "audio_end", "session_id": "s2"})

	messages := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
	assert.Equal(t, "s2", findByType(messages, "audio_stored")["session_id"])
	assert.Equal(t, []byte("second"), storage.audio(1))
}

func TestWebSocketRecordingConcurrentConnections(t *testing.T) {
	stt := &testSTT{}
	storage := &testStorage{wait: 10 * time.Millisecond}
	ts := startTestServer(t, stt, storage)

	// 三条连接各自录音，互不干扰。
	const connCount = 3
	conns := make([]*websocket.Conn, connCount)
	inboxes := make([]*inbox, connCount)
	for i := 0; i < connCount; i++ {
		conns[i] = dialWS(t, ts)
		inboxes[i] = newInbox(conns[i])
	}

	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(idx int, c *websocket.Conn) {
			defer wg.Done()
			sessionID := "conn-" + strconv.Itoa(idx)
			// 子 goroutine 中不能用 require（FailNow 只能作用于测试 goroutine）。
			if !assert.NoError(t, c.WriteJSON(map[string]interface{}{"type": "audio_start", "session_id": sessionID})) {
				return
			}
			for j := 0; j < 5; j++ {
				data, _ := json.Marshal(map[string]int{"conn": idx, "chunk": j})
				if !assert.NoError(t, c.WriteMessage(websocket.BinaryMessage, data)) {
					return
				}
			}
			assert.NoError(t, c.WriteJSON(map[string]interface{}{
				"type": "audio_end", "session_id": sessionID, "one_shot": true,
			}))
		}(i, c)
	}

	for i, in := range inboxes {
		messages := in.expectTypes(t, recvTimeout, "audio_stored", "recognition_complete", "session_end")
		assert.Equal(t, "conn-"+strconv.Itoa(i), findByType(messages, "audio_stored")["session_id"])
		in.expectClosed(t, recvTimeout)
	}
	wg.Wait()

	assert.Equal(t, connCount, storage.calls())
	assert.Equal(t, connCount, stt.calls())
}
