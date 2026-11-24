package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionManagerEndSessionReturnsError 验证兼容封装的 EndSession 不会吞掉
// 无效会话的错误（旧实现会返回 "session not found"）。
func TestSessionManagerEndSessionReturnsError(t *testing.T) {
	sm := NewSessionManager()

	// 会话不存在：必须返回错误，而不是静默返回 nil。
	err := sm.EndSession("missing", nil)
	require.Error(t, err)

	// 未知会话也不会被当作活跃会话。
	assert.Empty(t, sm.GetCurrentSession())
}

func TestSessionManagerStartAndAppend(t *testing.T) {
	sm := NewSessionManager()

	sm.StartSession("s1")
	assert.Equal(t, "s1", sm.GetCurrentSession())

	require.NoError(t, sm.AppendAudioData("s1", []byte("audio")))
	// 不存在的会话追加数据应报错。
	assert.Error(t, sm.AppendAudioData("missing", []byte("x")))

	sm.CleanupSession("s1")
	// 清理后音频会被拒绝。
	assert.Error(t, sm.AppendAudioData("s1", []byte("late")))
	assert.Empty(t, sm.GetCurrentSession())
}
