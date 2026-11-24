package recording

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStorage 是 storage.Service 的内存实现，可配置返回错误与阻塞。
type fakeStorage struct {
	mu    sync.Mutex
	calls [][]byte
	err   error
	urls  []string
	// block 非空时 StoreAudio 会等待该 channel 关闭/发送后再返回，用于制造并发交错。
	block chan struct{}
	// entered 在 StoreAudio 开始阻塞前收到一个信号。
	entered chan struct{}
}

func (f *fakeStorage) StoreAudio(audioData []byte) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]byte(nil), audioData...))
	callIndex := len(f.calls)
	err := f.err
	block := f.block
	entered := f.entered
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return "", err
	}
	return "https://storage.local/audio/" + strconv.Itoa(callIndex) + ".wav", nil
}

func (f *fakeStorage) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeStorage) audioAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// fakeSTT 是 stt.Service 的内存实现，可通过 audioData 映射返回固定文本。
type fakeSTT struct {
	mu    sync.Mutex
	calls [][]byte
	err   error
	texts map[string]string
	// block 非空时 Recognize 会阻塞，用于制造并发交错。
	block   chan struct{}
	entered chan struct{}
}

func (f *fakeSTT) Recognize(audioData []byte, audioURL string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]byte(nil), audioData...))
	err := f.err
	texts := f.texts
	block := f.block
	entered := f.entered
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return "", err
	}
	if texts != nil {
		if text, ok := texts[string(audioData)]; ok {
			return text, nil
		}
	}
	return "transcript:" + string(audioData), nil
}

func (f *fakeSTT) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeWriter 记录发送的结果消息，模拟客户端连接。
type fakeWriter struct {
	mu       sync.Mutex
	messages []map[string]interface{}
	closed   bool
	// messagesAtClose 记录调用 Close 时已发送的消息条数，用于证明确实是先发消息、
	// 后关闭连接。
	messagesAtClose int
	writeErr        error
	closeErr        error
}

func (w *fakeWriter) WriteJSON(v interface{}) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("write on closed connection")
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	w.messages = append(w.messages, msg)
	return nil
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messagesAtClose = len(w.messages)
	w.closed = true
	return w.closeErr
}

func (w *fakeWriter) messagesWhenClosed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.messagesAtClose
}

func (w *fakeWriter) snapshot() []map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]map[string]interface{}, len(w.messages))
	copy(out, w.messages)
	return out
}

func (w *fakeWriter) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *fakeWriter) types() []string {
	msgs := w.snapshot()
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i], _ = m["type"].(string)
	}
	return out
}

func (w *fakeWriter) find(msgType string) map[string]interface{} {
	for _, m := range w.snapshot() {
		if m["type"] == msgType {
			return m
		}
	}
	return nil
}

func intToString(i int) string {
	return strconv.Itoa(i)
}

func TestManagerEndSuccess(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{texts: map[string]string{"hello audio": "hello world"}}
	manager := NewManager(stt, storage)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("hello audio")))

	writer := &fakeWriter{}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	require.NotNil(t, outcome)

	assert.NoError(t, outcome.StorageErr)
	assert.NoError(t, outcome.RecognizeErr)
	assert.Equal(t, "https://storage.local/audio/1.wav", outcome.AudioURL)
	assert.Equal(t, "hello world", outcome.Text)

	assert.Equal(t, 1, storage.callCount())
	assert.Equal(t, []byte("hello audio"), storage.audioAt(0))
	assert.Equal(t, 1, stt.callCount())

	assert.Equal(t, []string{"audio_stored", "recognition_complete", "session_end"}, writer.types())

	// 非一次性会话不应关闭连接。
	assert.False(t, writer.isClosed())

	stored := writer.find("audio_stored")
	require.NotNil(t, stored)
	assert.Equal(t, "s1", stored["session_id"])
	assert.Equal(t, outcome.AudioURL, stored["audio_url"])

	recognized := writer.find("recognition_complete")
	require.NotNil(t, recognized)
	assert.Equal(t, "s1", recognized["session_id"])
	assert.Equal(t, "hello world", recognized["text"])
}

func TestManagerOneShotClosesAfterResults(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	require.NoError(t, manager.Start("s1", true))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	_, err := manager.End("s1", writer)
	require.NoError(t, err)

	assert.True(t, writer.isClosed(), "一次性会话应在结果发送后关闭连接")
	// 关闭发生在所有结果消息之后：到最后仍能读到三条消息。
	assert.Equal(t, []string{"audio_stored", "recognition_complete", "session_end"}, writer.types())
	// 直接证明：调用 Close 时三条结果消息都已经写出。
	assert.Equal(t, 3, writer.messagesWhenClosed(), "关闭连接前必须已发出全部结果消息")
}

func TestManagerStorageFailure(t *testing.T) {
	storage := &fakeStorage{err: errors.New("bucket unavailable")}
	manager := NewManager(&fakeSTT{}, storage)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	require.NotNil(t, outcome)
	assert.Error(t, outcome.StorageErr)

	storageErr := writer.find("storage_error")
	require.NotNil(t, storageErr, "存储失败必须回传明确错误消息")
	assert.Equal(t, "s1", storageErr["session_id"])
	assert.Contains(t, storageErr["error"], "bucket unavailable")

	// 存储失败不应阻止识别结果继续送达。
	require.NotNil(t, writer.find("recognition_complete"))

	assert.Equal(t, []string{"storage_error", "recognition_complete", "session_end"}, writer.types())
	assert.False(t, writer.isClosed())
}

func TestManagerRecognitionFailure(t *testing.T) {
	stt := &fakeSTT{err: errors.New("stt quota exceeded")}
	manager := NewManager(stt, &fakeStorage{})

	require.NoError(t, manager.Start("s1", true))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	require.NotNil(t, outcome)
	assert.Error(t, outcome.RecognizeErr)

	recognizeErr := writer.find("recognition_error")
	require.NotNil(t, recognizeErr, "识别失败必须回传明确错误消息")
	assert.Equal(t, "s1", recognizeErr["session_id"])
	assert.Contains(t, recognizeErr["error"], "stt quota exceeded")

	// 存储结果仍应送达。
	require.NotNil(t, writer.find("audio_stored"))

	assert.Equal(t, []string{"audio_stored", "recognition_error", "session_end"}, writer.types())
	assert.True(t, writer.isClosed(), "一次性会话在失败消息发出后也应关闭")
}

func TestManagerBothFailuresStillRespond(t *testing.T) {
	manager := NewManager(
		&fakeSTT{err: errors.New("stt down")},
		&fakeStorage{err: errors.New("storage down")},
	)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	assert.Error(t, outcome.StorageErr)
	assert.Error(t, outcome.RecognizeErr)

	assert.Equal(t, []string{"storage_error", "recognition_error", "session_end"}, writer.types())
}

func TestManagerDuplicateEndDoesNotRepeatWork(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{}
	manager := NewManager(stt, storage)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	_, err := manager.End("s1", writer)
	require.NoError(t, err)

	// 第二次结束必须被拒绝，且不产生额外的上传/识别。
	_, err = manager.End("s1", writer)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionEnded)

	assert.Equal(t, 1, storage.callCount(), "重复结束不得重复上传")
	assert.Equal(t, 1, stt.callCount(), "重复结束不得重复识别")
	assert.Equal(t, []string{"audio_stored", "recognition_complete", "session_end"}, writer.types())
}

func TestManagerConcurrentDuplicateEndRunsOnce(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{}
	manager := NewManager(stt, storage)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	const goroutines = 8
	var wg sync.WaitGroup
	results := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := manager.End("s1", writer)
			results[idx] = err
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		} else {
			assert.ErrorIs(t, err, ErrSessionEnded)
		}
	}
	assert.Equal(t, 1, successes, "并发结束信号只能有一个生效")
	assert.Equal(t, 1, storage.callCount())
	assert.Equal(t, 1, stt.callCount())
}

func TestManagerAppendAfterEndRejected(t *testing.T) {
	storageSvc := &fakeStorage{}
	manager := NewManager(&fakeSTT{}, storageSvc)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("first")))

	writer := &fakeWriter{}
	_, err := manager.End("s1", writer)
	require.NoError(t, err)

	// 会话结束后再次收到音频数据必须被拒绝，而不是写入已释放的缓冲。
	err = manager.Append("s1", []byte("late audio"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionEnded)

	// 已释放的缓冲内容不应被后续数据污染。
	assert.Equal(t, []byte("first"), storageSvc.audioAt(0))
}

func TestManagerUnknownSessionRejected(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	assert.ErrorIs(t, manager.Append("missing", []byte("x")), ErrSessionNotFound)

	writer := &fakeWriter{}
	_, err := manager.End("missing", writer)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionNotFound)
	assert.Empty(t, writer.snapshot())
}

func TestManagerInterleavedSessionsIsolated(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{texts: map[string]string{
		"session-a":  "text a",
		"session-b!": "text b",
	}}
	manager := NewManager(stt, storage)

	// 同一连接上先后开始两个会话，并交错写入音频。
	require.NoError(t, manager.Start("a", false))
	require.NoError(t, manager.Start("b", true))
	require.NoError(t, manager.Append("a", []byte("session-")))
	require.NoError(t, manager.Append("b", []byte("session-")))
	require.NoError(t, manager.Append("a", []byte("a")))
	require.NoError(t, manager.Append("b", []byte("b")))

	writer := &fakeWriter{}
	_, err := manager.End("a", writer)
	require.NoError(t, err)

	// 结束 a 不应清掉 b 的缓冲，也不应影响 b 的活跃状态。
	assert.Equal(t, []string{"b"}, manager.ActiveSessions())
	require.NoError(t, manager.Append("b", []byte("!")))

	writerB := &fakeWriter{}
	_, err = manager.End("b", writerB)
	require.NoError(t, err)

	assert.Equal(t, 2, storage.callCount())
	// 两次上传的内容必须各自完整且互不串台。
	audioA := string(storage.audioAt(0))
	audioB := string(storage.audioAt(1))
	assert.Equal(t, "session-a", audioA)
	assert.Equal(t, "session-b!", audioB)

	storedA := writer.find("audio_stored")
	require.NotNil(t, storedA)
	assert.Equal(t, "a", storedA["session_id"])
	recognizedA := writer.find("recognition_complete")
	require.NotNil(t, recognizedA)
	assert.Equal(t, "text a", recognizedA["text"])

	storedB := writerB.find("audio_stored")
	require.NotNil(t, storedB)
	assert.Equal(t, "b", storedB["session_id"])
	recognizedB := writerB.find("recognition_complete")
	require.NotNil(t, recognizedB)
	assert.Equal(t, "text b", recognizedB["text"])

	// a 不是一次性会话，b 是一次性会话：只有 b 应关闭连接。
	assert.False(t, writer.isClosed())
	assert.True(t, writerB.isClosed())
}

func TestManagerConcurrentSessionsDoNotInterfere(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{}
	manager := NewManager(stt, storage)

	const sessions = 5
	for i := 0; i < sessions; i++ {
		require.NoError(t, manager.Start(sessionID(i), false))
	}

	var appendWg sync.WaitGroup
	appendWg.Add(sessions)
	for i := 0; i < sessions; i++ {
		go func(idx int) {
			defer appendWg.Done()
			for j := 0; j < 20; j++ {
				assert.NoError(t, manager.Append(sessionID(idx), []byte{byte('a' + idx)}))
			}
		}(i)
	}
	appendWg.Wait()

	var endWg sync.WaitGroup
	endWg.Add(sessions)
	writers := make([]*fakeWriter, sessions)
	for i := 0; i < sessions; i++ {
		writers[i] = &fakeWriter{}
		go func(idx int) {
			defer endWg.Done()
			_, err := manager.End(sessionID(idx), writers[idx])
			assert.NoError(t, err)
		}(i)
	}
	endWg.Wait()

	assert.Equal(t, sessions, storage.callCount())
	assert.Equal(t, sessions, stt.callCount())
	assert.Empty(t, manager.ActiveSessions())

	// 每个会话上传的音频都应是它自己的内容，长度一致且不含其它会话数据。
	// 收尾是并发的，因此按内容而不是调用顺序归类。
	seen := make(map[byte]bool, sessions)
	for _, call := range storage.calls {
		require.Len(t, call, 20)
		first := call[0]
		require.False(t, seen[first], "同一个会话不应被上传两次")
		seen[first] = true
		for _, b := range call {
			assert.Equal(t, first, b, "会话之间不得串台")
		}
	}
	assert.Len(t, seen, sessions)

	for i, w := range writers {
		recognized := w.find("recognition_complete")
		require.NotNil(t, recognized)
		assert.Equal(t, sessionID(i), recognized["session_id"])
		assert.True(t, strings.HasPrefix(recognized["text"].(string), "transcript:"))
	}
}

func TestManagerSetOneShotOnActiveSession(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.SetOneShot("s1", true))

	writer := &fakeWriter{}
	_, err := manager.End("s1", writer)
	require.NoError(t, err)
	assert.True(t, writer.isClosed())
}

func TestManagerSessionRestartRejected(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	require.NoError(t, manager.Start("s1", false))
	assert.ErrorIs(t, manager.Start("s1", false), ErrSessionExists)
	assert.ErrorIs(t, manager.Start("", false), ErrEmptySessionID)

	writer := &fakeWriter{}
	_, err := manager.End("s1", writer)
	require.NoError(t, err)

	// 已结束的会话 ID 不能被重新用来录音（避免旧的收尾状态串到新会话）。
	assert.ErrorIs(t, manager.Start("s1", false), ErrSessionEnded)
}

func TestManagerCleanupReleasesBuffer(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	manager.Cleanup("s1")

	assert.ErrorIs(t, manager.Append("s1", []byte("late")), ErrSessionEnded)
	assert.Empty(t, manager.ActiveSessions())
}

func TestManagerWriteErrorStopsFurtherWrites(t *testing.T) {
	storage := &fakeStorage{}
	stt := &fakeSTT{}
	manager := NewManager(stt, storage)

	require.NoError(t, manager.Start("s1", true))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{writeErr: errors.New("broken pipe")}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	assert.Error(t, outcome.WriteErr)
	// 写失败但一次性会话仍应被关闭，避免连接悬挂。
	assert.True(t, writer.isClosed())
	// 存储与识别仍会执行一次，结果由 outcome 体现。
	assert.Equal(t, 1, storage.callCount())
	assert.Equal(t, 1, stt.callCount())
}

func TestManagerOneShotDoesNotCloseWhileOtherSessionActive(t *testing.T) {
	manager := NewManager(&fakeSTT{}, &fakeStorage{})

	// 同一连接上：A 仍在录音，B 是一次性会话。
	require.NoError(t, manager.Start("A", false))
	require.NoError(t, manager.Start("B", true))
	require.NoError(t, manager.Append("A", []byte("a")))
	require.NoError(t, manager.Append("B", []byte("b")))

	// 两者共用同一个连接写入器，模拟真实的一条 WebSocket 连接。
	writer := &fakeWriter{}

	// 结束一次性会话 B：不能因此关闭连接，A 仍在录音。
	_, err := manager.End("B", writer)
	require.NoError(t, err)
	assert.False(t, writer.isClosed(), "另一会话仍在录音时，一次性会话不得关闭连接")
	assert.Equal(t, []string{"A"}, manager.ActiveSessions())

	// 继续向 A 写入音频并结束 A；此时连接才可以关闭。
	require.NoError(t, manager.Append("A", []byte("2")))
	_, err = manager.End("A", writer)
	require.NoError(t, err)
	assert.True(t, writer.isClosed(), "所有会话结束后，延迟的一次性关闭才生效")
}

func TestManagerSlowRecognitionDoesNotBlockStorageResult(t *testing.T) {
	recognitionBlock := make(chan struct{})
	stt := &fakeSTT{block: recognitionBlock, entered: make(chan struct{}, 1)}
	manager := NewManager(stt, &fakeStorage{})

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.End("s1", writer)
	}()

	// 等待识别进入阻塞状态，同时确认存储结果已经送达。
	select {
	case <-stt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("识别未开始")
	}

	deadline := time.After(2 * time.Second)
	for {
		if writer.find("audio_stored") != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("存储结果被慢识别阻塞，未能及时送达")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// 释放识别，收尾完成。
	close(recognitionBlock)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("收尾未完成")
	}

	assert.Equal(t, []string{"audio_stored", "recognition_complete", "session_end"}, writer.types())
}

func TestManagerNilServicesReportFailure(t *testing.T) {
	manager := NewManager(nil, nil)

	require.NoError(t, manager.Start("s1", false))
	require.NoError(t, manager.Append("s1", []byte("audio")))

	writer := &fakeWriter{}
	outcome, err := manager.End("s1", writer)
	require.NoError(t, err)
	assert.Error(t, outcome.StorageErr)
	assert.Error(t, outcome.RecognizeErr)

	assert.Equal(t, []string{"storage_error", "recognition_error", "session_end"}, writer.types())
}

func sessionID(i int) string {
	return "session-" + intToString(i)
}
