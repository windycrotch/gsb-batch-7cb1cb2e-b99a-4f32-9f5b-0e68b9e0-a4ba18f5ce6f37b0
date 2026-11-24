package message

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStorage struct {
	mu   sync.Mutex
	data [][]byte
	err  error
}

func (f *fakeStorage) StoreAudio(audioData []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, append([]byte(nil), audioData...))
	if f.err != nil {
		return "", f.err
	}
	return "https://fake.storage/audio/1.wav", nil
}

func (f *fakeStorage) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}

type fakeSTT struct {
	mu   sync.Mutex
	err  error
	text string
	n    int
}

func (f *fakeSTT) Recognize(audioData []byte, audioURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func (f *fakeSTT) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

type fakeWriter struct {
	mu       sync.Mutex
	messages []map[string]interface{}
	closed   bool
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
		return errors.New("closed")
	}
	w.messages = append(w.messages, msg)
	return nil
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *fakeWriter) types() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.messages))
	for i, m := range w.messages {
		out[i], _ = m["type"].(string)
	}
	return out
}

func TestHandleEndDeliversResultsThenCloses(t *testing.T) {
	storageSvc := &fakeStorage{}
	sttSvc := &fakeSTT{text: "识别结果"}
	handler := NewBinaryMessageHandler(sttSvc, nil, storageSvc)

	require.NoError(t, handler.HandleStart("s1", true))
	require.NoError(t, handler.HandleAudioData("s1", []byte("audio")))

	writer := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s1", writer))

	assert.Equal(t, []string{"audio_stored", "recognition_complete", "session_end"}, writer.types())
	assert.True(t, writer.closed, "一次性会话应在结果发送后关闭")
}

func TestHandleEndDuplicateIgnored(t *testing.T) {
	storageSvc := &fakeStorage{}
	sttSvc := &fakeSTT{text: "x"}
	handler := NewBinaryMessageHandler(sttSvc, nil, storageSvc)

	require.NoError(t, handler.HandleStart("s1", false))
	require.NoError(t, handler.HandleAudioData("s1", []byte("audio")))

	writer := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s1", writer))

	// 重复结束返回错误且不重复上传/识别。
	require.Error(t, handler.handleEnd("s1", writer))
	assert.Equal(t, 1, storageSvc.calls())
	assert.Equal(t, 1, sttSvc.calls())
}

func TestHandleEndFailuresAreReported(t *testing.T) {
	storageSvc := &fakeStorage{err: errors.New("storage boom")}
	sttSvc := &fakeSTT{err: errors.New("stt boom")}
	handler := NewBinaryMessageHandler(sttSvc, nil, storageSvc)

	require.NoError(t, handler.HandleStart("s1", false))
	require.NoError(t, handler.HandleAudioData("s1", []byte("audio")))

	writer := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s1", writer))

	assert.Equal(t, []string{"storage_error", "recognition_error", "session_end"}, writer.types())
	assert.False(t, writer.closed)
}

func TestOneShotIsPerSessionNotGlobal(t *testing.T) {
	handler := NewBinaryMessageHandler(&fakeSTT{}, nil, &fakeStorage{})

	// s1 是一次性会话，s2 不是。s2 的结束不应被 s1 的 one_shot 标记影响。
	require.NoError(t, handler.HandleStart("s1", true))
	require.NoError(t, handler.HandleStart("s2", false))
	require.NoError(t, handler.HandleAudioData("s1", []byte("a")))
	require.NoError(t, handler.HandleAudioData("s2", []byte("b")))

	// 先结束非一次性的 s2：它绝不应导致连接关闭。
	writer2 := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s2", writer2))
	assert.False(t, writer2.closed, "s2 不是一次性会话，不应被 s1 的设置影响")

	// 再结束一次性的 s1；此时已无其它会话，才允许关闭。
	writer1 := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s1", writer1))
	assert.True(t, writer1.closed)
}

func TestOneShotCloseDeferredWhileOtherSessionRecording(t *testing.T) {
	handler := NewBinaryMessageHandler(&fakeSTT{}, nil, &fakeStorage{})

	require.NoError(t, handler.HandleStart("one", true))
	require.NoError(t, handler.HandleStart("live", false))
	require.NoError(t, handler.HandleAudioData("one", []byte("a")))
	require.NoError(t, handler.HandleAudioData("live", []byte("b")))

	// 一次性会话结束，但另一会话仍在录音：此时不得关闭连接。
	writerOne := &fakeWriter{}
	require.NoError(t, handler.handleEnd("one", writerOne))
	assert.False(t, writerOne.closed, "仍有会话在录音时不得关闭连接")

	// 另一会话结束后，延迟的一次性关闭才生效。
	writerLive := &fakeWriter{}
	require.NoError(t, handler.handleEnd("live", writerLive))
	assert.False(t, writerLive.closed, "发起一次性关闭的是 one 的写入器，不应关闭 live 的写入器")
	assert.True(t, writerOne.closed, "所有会话结束后应关闭发起一次性关闭的写入器")
}

func TestAudioAfterEndRejected(t *testing.T) {
	handler := NewBinaryMessageHandler(&fakeSTT{}, nil, &fakeStorage{})

	require.NoError(t, handler.HandleStart("s1", false))
	require.NoError(t, handler.HandleAudioData("s1", []byte("audio")))

	writer := &fakeWriter{}
	require.NoError(t, handler.handleEnd("s1", writer))

	require.Error(t, handler.HandleAudioData("s1", []byte("late")))
}
