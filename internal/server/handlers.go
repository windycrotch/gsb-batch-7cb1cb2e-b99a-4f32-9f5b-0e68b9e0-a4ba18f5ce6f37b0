package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/telepace/voiceflow/internal/server/recording"
	"github.com/telepace/voiceflow/pkg/config"
	"github.com/telepace/voiceflow/pkg/logger"

	"github.com/gorilla/websocket"
	"github.com/telepace/voiceflow/internal/storage"
	"github.com/telepace/voiceflow/internal/stt"
	"github.com/telepace/voiceflow/internal/tts"
)

var (
	// 服务实例和锁
	serviceLock sync.RWMutex
	sttService  stt.Service
	ttsService  tts.Service
	// llmService     llm.Service
	storageService storage.Service
)

// 初始化服务实例
func InitServices() {
	cfg, err := config.GetConfig()
	if err != nil {
		logger.Fatalf("配置初始化失败: %v", err)
	}
	sttService, err = stt.NewService(cfg.STT.Provider)
	if err != nil {
		logger.Fatalf("STT 服务初始化失败: %v", err)
	}
	ttsService = tts.NewService(cfg.TTS.Provider)
	// llmService = llm.NewService(cfg.LLM.Provider)
	storageService = storage.NewService()
}

// 修改消息结构
type TextMessage struct {
	Text       string `json:"text"`
	RequireTTS bool   `json:"require_tts"`
}

// audioControlMessage 是音频会话控制消息（audio_start / audio_end）。
type audioControlMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	OneShot   bool   `json:"one_shot"`
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket upgrade error: %v", err)
		return
	}

	writer := newConnWriter(ws)
	defer ws.Close()

	// 每条连接拥有独立的会话管理器，多个会话的缓冲与结果互不干扰。
	manager := recording.NewManager(sttService, storageService)
	defer manager.ReleaseAll()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.Info("WebSocket connection closed normally")
			} else {
				logger.Error("WebSocket read error", "error", err)
			}
			break
		}

		switch mt {
		case websocket.TextMessage:
			var msg map[string]interface{}
			if err := json.Unmarshal(data, &msg); err != nil {
				logger.Error("解析消息失败", "error", err)
				continue
			}

			// 检查是否是音频相关的控制消息
			if msgType, ok := msg["type"].(string); ok && isAudioControl(msgType) {
				var control audioControlMessage
				if err := json.Unmarshal(data, &control); err != nil {
					logger.Error("解析音频控制消息失败", "error", err)
					continue
				}
				switch control.Type {
				case "audio_start":
					if err := manager.Start(control.SessionID, control.OneShot); err != nil {
						logger.Error("开始录音会话失败", "session_id", control.SessionID, "error", err)
					}
				case "audio_end":
					// 在读循环中同步摘取会话（标记结束并释放缓冲），
					// 保证之后到达的音频被拒绝、重复结束只处理一次；
					// 耗时的存储/识别/回传放到后台完成。
					handleAudioEnd(manager, writer, control.SessionID, control.OneShot)
				}
				continue
			}

			// 处理普通文本消息
			text, _ := msg["text"].(string)
			requireTTS, _ := msg["require_tts"].(bool)

			if requireTTS {
				// 调用 TTS 服务
				audio, err := ttsService.Synthesize(text)
				if err != nil {
					logger.Error("语音合成失败", "error", err)
					continue
				}

				// 存储音频文件
				audioURL, err := storageService.StoreAudio(audio)
				if err != nil {
					logger.Error("存储音频失败", "error", err)
					continue
				}

				// 发送响应给客户端
				response := map[string]interface{}{
					"type":      "tts_complete",
					"text":      text,
					"audio_url": audioURL,
				}

				if err := writer.WriteJSON(response); err != nil {
					logger.Error("发送响应失败", "error", err)
				}
			}

		case websocket.BinaryMessage:
			sessionID, err := manager.AppendCurrent(data)
			if err != nil {
				logger.Error("追加音频数据失败", "session_id", sessionID, "error", err)
			}
		}
	}
}

// handleAudioEnd 完成会话收尾。摘取会话的步骤在读循环中同步执行，
// 因此重复的结束信号只会在 manager 内部被接受一次，不会重复上传或重复识别；
// 耗时的存储、识别和结果回传在后台 goroutine 中进行。
func handleAudioEnd(manager *recording.Manager, writer *connWriter, sessionID string, oneShot bool) {
	// 允许客户端只在结束时携带 one_shot 标记。
	if oneShot {
		if err := manager.SetOneShot(sessionID, true); err != nil {
			logger.Debugf("更新会话 one_shot 标记失败: %v", err)
		}
	}

	pending, err := manager.BeginEnd(sessionID)
	if err != nil {
		// 多数情况下是重复的 audio_end 或无效会话 ID，不需要回传消息。
		logger.Debugf("忽略会话结束信号 session_id=%s: %v", sessionID, err)
		return
	}

	go func() {
		outcome := pending.Finalize(writer)
		if outcome.StorageErr != nil {
			logger.Errorf("会话 %s 音频存储失败: %v", sessionID, outcome.StorageErr)
		}
		if outcome.RecognizeErr != nil {
			logger.Errorf("会话 %s 语音识别失败: %v", sessionID, outcome.RecognizeErr)
		}
	}()
}

func isAudioControl(msgType string) bool {
	switch msgType {
	case "audio_start", "audio_end":
		return true
	default:
		return false
	}
}

// 配置更新处理函数
func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service  string `json:"service"`
		Provider string `json:"provider"`
	}

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 更新配置，使用写锁保护
	serviceLock.Lock()
	defer serviceLock.Unlock()

	err = config.SetProvider(req.Service, req.Provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 根据新的配置重新初始化服务实例
	//initServices()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Configuration updated"))
}
