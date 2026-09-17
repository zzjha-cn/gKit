# ASR 实时语音识别 WebSocket 接口

前端把麦克风音频推给本服务，本服务转发给上游识别引擎（当前为阿里云百炼 DashScope），
把识别结果实时回推给前端。

```
前端 ──WebSocket──> asr_api（接入层：鉴权/路由） ──> asr_srv（网桥：状态机/名额）──WebSocket──> 上游 ASR
```

对接方只需要关心本文的「上行」「下行」两张表；时序、超时、心跳都是服务端保证的。

---

## 1. 建立连接

```
GET /asr/v1/stream?token=<TOKEN>
Upgrade: websocket
```

路径由挂载方决定（下面第 7 节），`/asr/v1/stream` 是约定的默认路径。

**鉴权**：服务端配了 `token` 时必传，两种方式等价：

| 方式 | 写法 | 场景 |
| --- | --- | --- |
| Query | `?token=xxx` | 浏览器（`WebSocket` API 不支持自定义请求头） |
| 请求头 | `Authorization: Bearer xxx` | 服务端、移动端、压测工具 |

**握手阶段的 HTTP 状态码**（这些都发生在 WebSocket 升级*之前*，客户端拿到的是普通 HTTP 响应，
不是 WebSocket 关闭帧 —— 这样前端能明确区分「没让你连」和「连上又断了」）：

| 状态码 | 含义 | 处理建议 |
| --- | --- | --- |
| `101` | 升级成功 | 进入下面的会话流程 |
| `401` | token 缺失或错误 | 检查配置，不要重试 |
| `403` | Origin 不在白名单 | 联系服务端加 `allow_origins`，不要重试 |
| `503` | 并发已满 / ASR 未配置 | 退避后重试（建议 1s、2s、4s） |
| `400` / `405` | 不是合法的 WebSocket 升级请求 | 客户端 bug |

> 跨域默认**拒绝**。浏览器会带 `Origin`，所以前端域名必须写进服务端的 `allow_origins`；
> 非浏览器客户端不带 `Origin`，直接放行。

**请求标识**：建议带上 `X-Request-Id`（或 `X-Trace-Id` / `X-Correlation-Id`），
它会贯穿本服务与上游的全部日志，排查时报这一个 ID 就够。不带则服务端生成 UUID。

---

## 2. 上行：客户端 → 服务端

### 控制指令（文本帧，JSON）

按 `action` 字段路由。

#### `start` — 开始一次转录

必须是连接建立后的第一条消息。**只有第一次 `start` 生效**，重复发送会被忽略
（想换参数请断开重连）。

```json
{
  "action": "start",
  "format": "pcm",
  "sample_rate": 16000,
  "language_hints": ["zh", "en"],
  "semantic_punctuation": true,
  "max_sentence_silence": 1300
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `action` | string | ✅ | 固定 `"start"` |
| `format` | string | | 音频格式，默认 `pcm`；可选 `wav` / `mp3` / `opus` / `speex` / `aac` / `amr` |
| `sample_rate` | int | | 采样率，默认取服务端配置（通常 16000） |
| `language_hints` | []string | | 语种提示，如 `["zh"]`；留空由上游自动判别 |
| `semantic_punctuation` | bool | | 语义断句标点，不传用服务端默认值 |
| `max_sentence_silence` | int | | VAD 断句静音阈值，毫秒，有效范围 200–6000，不传用服务端默认值（1300） |

发出 `start` 后等 `ready` 事件再开始推音频。收到 `ready` 之前推的音频会进入缓冲队列，
但队列有上限（见第 5 节），别抢跑太多。

#### `stop` — 结束本次转录

```json
{ "action": "stop" }
```

服务端会把缓冲区里剩余的音频送完，再通知上游收尾，最后回一个 `done`。
**发完 `stop` 不要立刻关连接**，否则拿不到最后一句 `final` 和 `done`。

### 音频数据（二进制帧）

- 默认格式：**PCM16LE / 单声道**，采样率与 `start` 中的 `sample_rate` 一致
- 推荐每帧 **100ms**：16kHz 下即 3200 字节
- 单帧上限 **64KB**，超过会被直接断开（关闭码 `1009`）

不要把音频塞进 JSON（base64 会白涨 33% 体积并增加延迟），二进制帧直接发。

---

## 3. 下行：服务端 → 客户端

全部是文本帧，统一信封：

```json
{ "event": "<事件名>", "data": { ... } }
```

| `event` | `data` | 时机 |
| --- | --- | --- |
| `ready` | `{"session_id":"..."}` | 上游会话已就绪，**可以开始推音频** |
| `partial` | 句子结构（见下） | 句子中间结果，同一 `sentence_id` 会多次下发，文本逐步变长 |
| `final` | 句子结构（见下） | 句子定稿，该 `sentence_id` 不会再更新 |
| `done` | `{"duration_sec":7}` | 本次转录正常结束（计费时长，秒）。终态 |
| `error` | `{"code":500,"msg":"...","upstream_code":"..."}` | 出错。终态 |

句子结构（`partial` / `final` 共用）：

```json
{ "sentence_id": 1, "text": "今天天气不错。", "begin_ms": 120, "end_ms": 980 }
```

`begin_ms` / `end_ms` 是相对本次会话开始的毫秒偏移。

**渲染建议**：按 `sentence_id` 维护一个映射，`partial` 覆盖同 id 的文本（不要追加），
`final` 落定后开一个新 id。

`done` 是**终态**，服务端随后主动关闭连接。

`error` 除一个例外外也是终态：`start` 负载解析失败（`invalid start payload`）时连接**不会**关闭，
本次 `start` 也不算生效，客户端修好 JSON 重发一次即可。其余 `error` 都伴随关闭。
判断方式很简单：连接没断就还能用。

服务端保证**一定会给一个终态**——即使上游直接消失、超时、或者出现没预料到的路径，
也会兜一个 `error` 出来，绝不静默断开。

### 错误码

`data.code` 是本服务的归类（不是 HTTP 状态码）：

| `code` | 典型 `msg` | 原因 | 客户端怎么办 |
| --- | --- | --- | --- |
| 400 | `invalid start payload` | `start` 不是合法 JSON | 修好重发 `start`（连接不会断） |
| 400 | `超时未收到 start 指令` | 连上后 `start_wait_seconds`（默认 30s）内没发 `start` | 别提前建连，或建连后立即 `start` |
| 500 | `会话超过最长时长限制` | 单次会话超过 `max_session_seconds`（默认 600s） | 分段录制，收到后重连续录 |
| 500 | 上游返回的错误信息 | 上游识别失败，`upstream_code` 是上游原始错误码 | 按 `upstream_code` 判断，多数可重连重试 |
| 500 | `音频上送失败，会话已中断` | 到上游的链路断了 | 重连重试 |
| 500 | `等待上游收尾超时` | 发了 `stop` 但上游在 `drain_wait_seconds`（默认 10s）内没收尾 | 已收到的 `final` 仍然有效，可直接采用 |
| 500 | `上游会话异常结束` | 上游连接中断且没给结果 | 重连重试 |

### WebSocket 关闭码

| 关闭码 | 含义 |
| --- | --- |
| `1000` | 正常结束（`done` / `error` 之后的收尾） |
| `1001` | 服务端下线，或会话超过最长时长 |
| `1008` | 超时未发 `start` |
| `1009` | 单帧超过 64KB |
| `1006` | 异常断开（网络问题，没有关闭帧） |

除 `1006`（网络层异常断开，服务端根本没机会说话）和 `1009`（超大帧由协议层直接拒掉）外，
关闭之前服务端一定先发过一条 `error` 或 `done` 说明原因——静默断开会让前端误判成网络故障，
排查成本极高，所以这条是硬约定。

---

## 4. 完整时序

```
客户端                                服务端
  │  ── GET /asr/v1/stream?token= ──>  │   鉴权、占名额、升级
  │  <──────── 101 ─────────────────   │
  │                                    │
  │  ── {"action":"start", ...} ────>  │   建立上游会话（内部自带重试）
  │  <──── {"event":"ready"} ────────  │   ★ 收到这个才开始推音频
  │                                    │
  │  ── <binary 3200B> ────────────>   │
  │  <──── {"event":"partial"} ─────   │
  │  ── <binary 3200B> ────────────>   │
  │  <──── {"event":"final"} ───────   │   一句说完
  │            ...                     │
  │  ── {"action":"stop"} ─────────>   │   送完缓冲 → 通知上游收尾
  │  <──── {"event":"final"} ───────   │   可能还有最后一句
  │  <──── {"event":"done"} ────────   │   终态
  │  <──────── close 1000 ──────────   │
```

静默期不需要客户端做任何事：服务端每 30s 发一次 WebSocket ping，
浏览器/标准客户端的 WS 栈会自动回 pong，链路就一直活着。
读超时是 60s —— 只要你的 WS 栈正常回 pong，就不会被误杀。

---

## 5. 服务端保证与限制

| 项 | 默认值 | 行为 |
| --- | --- | --- |
| 单帧上限 | 64KB | 超过即断开（`1009`） |
| 音频缓冲 | 50 帧（≈5s） | **队列满时丢最老的帧**，不阻塞、不无界堆积。网络抖动时宁愿丢几帧也不让延迟雪崩；丢帧数会记进服务端日志 |
| 等 `start` | 30s | 超时报错并关闭，防空连接占名额 |
| 单会话时长 | 600s | 到点报错并关闭（ASR 按时长计费，必须有硬上限） |
| `stop` 后等收尾 | 10s | 上游不应答就自己收场 |
| 并发上限 | 20 | 满了在握手阶段回 `503` |
| 心跳 | 30s ping / 60s 读超时 | 服务端主动 ping |
| 上游建连 | 3 次尝试 / 总预算 10s | 只在**还没发过音频**的建连阶段重试；会话中途一律不重试（重发音频会让识别结果错乱） |

一条前端连接 ↔ 一路上游会话，1:1，不复用。

---

## 6. 客户端示例

### 浏览器

```js
const ws = new WebSocket(`wss://example.com/asr/v1/stream?token=${TOKEN}`);
ws.binaryType = 'arraybuffer';

const sentences = new Map();

ws.onopen = () => ws.send(JSON.stringify({
  action: 'start',
  format: 'pcm',
  sample_rate: 16000,
  language_hints: ['zh'],
}));

ws.onmessage = (e) => {
  const { event, data } = JSON.parse(e.data);
  switch (event) {
    case 'ready':
      startMic();                                  // ★ 到这里才开始采集/推流
      break;
    case 'partial':
    case 'final':
      sentences.set(data.sentence_id, data.text);   // 覆盖，不是追加
      render([...sentences.values()].join(''));
      break;
    case 'done':
      console.log('计费时长', data.duration_sec);
      break;
    case 'error':
      console.error(`[${data.code}] ${data.msg}`, data.upstream_code ?? '');
      break;
  }
};

// 采集侧：每 100ms 一帧 PCM16LE，直接发二进制
function onAudioFrame(int16Array) {
  if (ws.readyState === WebSocket.OPEN) ws.send(int16Array.buffer);
}

// 说完了：先 stop，等 done 再关
function finish() { ws.send(JSON.stringify({ action: 'stop' })); }
```

### Go

```go
header := http.Header{}
header.Set("Authorization", "Bearer "+token)
header.Set("X-Request-Id", reqID)

c, _, err := websocket.DefaultDialer.Dial("wss://example.com/asr/v1/stream", header)
if err != nil {
    return err
}
defer c.Close()

if err = c.WriteJSON(map[string]any{"action": "start", "sample_rate": 16000}); err != nil {
    return err
}

go func() {
    for {
        _, data, err := c.ReadMessage()
        if err != nil {
            return
        }
        var env struct {
            Event string          `json:"event"`
            Data  json.RawMessage `json:"data"`
        }
        _ = json.Unmarshal(data, &env)
        // ready / partial / final / done / error
    }
}()

// 音频：二进制帧，100ms 一帧
_ = c.WriteMessage(websocket.BinaryMessage, pcmFrame)
// 收尾
_ = c.WriteJSON(map[string]any{"action": "stop"})
```

更完整的可运行链路（含假上游）见 `ws_test.go`。

---

## 7. 服务端挂载

```go
import (
    asrapi "github.com/zzjha-cn/gKit/pkg/asr/asr_api"
    asrsrv "github.com/zzjha-cn/gKit/pkg/asr/asr_srv"
)

// ctx 必须是进程级 ctx，不能是请求 ctx。
// cfg 为 nil（未配置 ASR）时返回 nil，接口一律回 503。
mgr := asrsrv.InitManager(procCtx, cfg)

mux.HandleFunc("/asr/v1/stream", asrapi.StreamV1)              // 用全局实例
mux.Handle("/asr/v1/stream", asrapi.NewWSHandler(mgr))         // 或显式注入

// 优雅退出：广播关闭并**等待**连接收敛，
// 只取消 ctx 不等的话，正在收尾的会话来不及把最后的结果和关闭帧发出去。
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
_ = mgr.CloseAll(shutdownCtx)
```

### 配置（`ASRCfg`，YAML）

```yaml
asr:
  # 上游
  region: cn-beijing                              # cn-beijing | ap-southeast-1
  workspace_id: ""                                # 拼进 wss 地址的子域
  api_key: ""                                     # 必填
  model: qwen-audio-3.0-asr-flash-streaming
  endpoint: ""                                    # 覆盖默认 wss 地址，留空按 region+workspace 拼

  # 接入
  token: ""                                       # 前端握手 token，为空表示不校验
  allow_origins: []                               # WS 来源白名单，支持 "https://a.com" / "a.com" / "*.a.com"
  max_concurrent: 20                              # 并发会话上限

  # 识别参数
  sample_rate: 16000
  max_sentence_silence: 1300                      # VAD 断句阈值，200-6000ms
  semantic_punctuation: false
  audio_buffer_frames: 50                         # 音频有界队列长度（≈5s）

  # 上游超时与重试
  upstream_dial_timeout: 4                        # 单次拨号+等 task-started，秒
  upstream_start_attempts: 3                      # 建连尝试次数（含首次）
  upstream_start_timeout: 10                      # 建连总预算，秒（覆盖全部重试）
  upstream_read_timeout: 60                       # 上游静默多久判定链路已死，秒

  # 会话闸门
  start_wait_seconds: 30                          # 连上后等 start 的秒数
  drain_wait_seconds: 10                          # 发出收尾指令后等上游的秒数
  max_session_seconds: 600                        # 单次会话最长时长
```

留空/零值会被 `Normalize()` 补成上面的默认值，唯一必填的是 `api_key`。

> `allow_origins` 为空时只允许同源请求。前端独立域名部署时**必须**配，否则浏览器握手会拿到 403。

### 加一种接入方式（如 gRPC）

`asr_srv` 不认识 HTTP，只提供中转能力，接入协议全在本包。加 gRPC 就在本包新建 `grpc.go`，
拿到双向流之后复用同一套流程：

```
mgr.Config()        读配置（token / 白名单 / 上限）
mgr.Acquire()       占名额（必须在建流之前，满了直接拒）
mgr.ConnOptions()   连接策略（超时/心跳/帧上限/时间闸，由中转层定义，接入层不要自己编一套）
mgr.Track(conn, release)
asrsrv.NewBridge(mgr, conn, reqID).Run()
```

`asr_srv` 一行都不用改。
