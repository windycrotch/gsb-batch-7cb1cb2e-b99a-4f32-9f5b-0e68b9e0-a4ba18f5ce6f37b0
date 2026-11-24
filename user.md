### 代码分析

### API 和 WebSocket 接口文档

#### 1. HTTP API 接口

##### 1.1 配置更新接口

- **URL**：`/config`
- **方法**：`POST`
- **描述**：用于更新服务器的服务配置，包括指定使用的服务类型和提供商。
- **请求头**：
    - `Content-Type: application/json`
- **请求体**：

  ```json
  {
    "service": "string",    // 服务名称，例如 "STT", "TTS", "LLM"
    "provider": "string"    // 提供商名称，例如 "Google", "AWS", "Azure"
  }
  ```

- **成功响应**：
    - **状态码**：`200 OK`
    - **响应体**：

      ```
      Configuration updated
      ```

- **错误响应**：
    - **状态码**：`400 Bad Request`
    - **响应体**：

      ```
      Invalid request body
      ```


#### 2. WebSocket 接口

##### 2.1 建立连接

- **URL**：`ws://<服务器地址>/ws`
- **协议**：WebSocket
- **描述**：客户端通过WebSocket与服务器建立连接，以进行实时的双向通信，包括文本处理和音频数据传输。

##### 2.2 消息类型

WebSocket连接支持两种类型的消息：

1. **文本消息**：
    - **格式**：JSON对象
    - **用途**：发送需要处理的文本，服务器会返回处理结果和相关音频URL。
    - **示例消息**：

      ```json
      {
        "text": "你好，今天的天气怎么样？"
      }
      ```

    - **服务器响应**：
        - **格式**：JSON对象，包含处理后的文本和音频文件的URL。
        - **示例响应**：

          ```json
          {
            "text": "今天天气晴朗，气温适中。",
            "audio_url": "http://example.com/audio/12345.mp3"
          }
          ```

2. **二进制消息**：
    - **格式**：二进制音频数据
    - **用途**：发送音频流，服务器将进行语音转文字（STT），并返回转录结果。
    - **服务器响应**：
        - **格式**：JSON对象，包含转录的文本或结束事件。
        - **示例响应**（转录中）：

          ```json
          {
            "event": "result",
            "result": {
              "Text": "这是转录的内容。"
            },
            "code": 0,
            "message": "这是转录的内容。"
          }
          ```

        - **示例响应**（结束）：

          ```json
          {
            "event": "end",
            "code": 0,
            "message": ""
          }
          ```

##### 2.3 消息流程

1. **文本处理流程**：
    - 客户端发送包含`text`字段的JSON消息。
    - 服务器接收后，调用LLM服务生成响应文本。
    - 使用TTS服务合成音频，并将音频存储后返回音频URL。
    - 服务器通过WebSocket发送包含响应文本和音频URL的JSON消息给客户端。

2. **音频转录流程**：
    - 客户端发送 `{"type": "audio_start", "session_id": "...", "one_shot": false}` 开始一次录音会话。
    - 客户端发送二进制音频数据；服务器按当前会话累积音频。
    - 客户端发送 `{"type": "audio_end", "session_id": "..."}` 结束会话。
    - 服务器并发完成「音频存储」和「语音识别」，并按固定顺序回传：
      1. `audio_stored`（成功，含 `audio_url`）或 `storage_error`（失败，含 `error`）；
      2. `recognition_complete`（成功，含 `text`）或 `recognition_error`（失败，含 `error`）；
      3. `session_end`（收尾完成，含 `one_shot`）。
    - 每个会话的结束只处理一次：重复的 `audio_end` 不会重复上传或重复识别。
    - 会话结束后其音频缓冲被释放，之后再发送该会话的二进制数据会被拒绝。
    - `one_shot` 为 `true` 时，服务器在上述结果消息全部发出后才关闭连接；
      若同一连接上仍有其它会话在录音或收尾，关闭会延后到它们全部完成，避免
      丢失其它会话的结果。


#### 3. 示例

##### 3.1 使用WebSocket进行文本交互

**客户端发送**：

```json
{
  "text": "请告诉我一个笑话。"
}
```

**服务器响应**：

```json
{
  "text": "当然，为什么程序员喜欢在夜晚工作？因为晚上调试错误更容易！",
  "audio_url": "http://example.com/audio/67890.mp3"
}
```

##### 3.2 使用WebSocket进行音频转录

**客户端发送**：二进制音频数据（例如录制的语音）

**服务器响应**：

```json
{
  "event": "result",
  "code": 0,
  "result": [
    {
        "definite": true,
        "end_time": 860,
        "start_time": 0,
        "text": "这是",
        "words": [
            {
            "blank_duration": 0,
            "end_time": 1020,
            "start_time": 860,
            "text": "这"
            },
            {
            "blank_duration": 0,
            "end_time": 1180,
            "start_time": 1020,
            "text": "是"
            }
        ],
        "word_size": 2
    },
    {
      "definite": true,
      "end_time": 1705,
      "start_time": 0,
      "text": "这是字节跳动，",
      "words": [
        {
          "blank_duration": 0,
          "end_time": 860,
          "start_time": 740,
          "text": "这"
        },
        {
          "blank_duration": 0,
          "end_time": 1020,
          "start_time": 860,
          "text": "是"
        }
      ],
      "word_size": 2
    }
  ]
}
```

**当音频传输结束**：

```json
{
  "event": "end",
  "code": 0,
  "message": ""
}
```
