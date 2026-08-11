# WebSocket 封装设计文档

基于 `github.com/gorilla/websocket` 的应用层封装。目标不是造一个 WebSocket 框架，
而是**把 gorilla 留给应用的那几个坑一次性填掉**，让业务代码不必反复手写同一套模板。

暂定包名 `pkg/wsx`。

---

## 1. 为什么要封装

gorilla 是一个刻意保持底层的库，它把三类问题明确推给了应用：

| 问题 | gorilla 的态度 | 后果 |
|---|---|---|
| 并发写 | 只用 `c.mu` 保护控制帧（`WriteControl` / `Close`），数据帧靠 `isWriting` **panic 探测** | 每个项目自己加锁 |
| 超时 | `SetReadDeadline` / `SetWriteDeadline` 是一次性的，不是策略 | 每次读写前重设，和心跳耦合 |
| 心跳 | 只在 example 里给了模板 | `pongWait` / `pingPeriod` / `SetPongHandler` 三件套被抄无数遍 |

注意第一条：`c.mu` 的存在容易让人误以为写是安全的。它只保证 ping/close 帧不会插进数据帧中间——
这正是官方文档里唯一的豁免（"The Close and WriteControl methods can be called concurrently
with all other methods"）。`WriteMessage` 之间共享 `writeBuf`、`writer`、`writeDeadline`，
完全不受保护，源码里那句注释写得很直白：**best-effort detection of concurrent writes**。

封装要解决的就是这些，**不多也不少**。

---

## 2. 设计原则

1. **补缺口，不造框架。** 每一个能力都要能指出"不封会重复写什么代码"。
2. **危险 API 不导出。** 最容易错的原语（`SetWriteDeadline`、裸 `WriteMessage`）直接关掉，
   而不是导出后在文档里叮嘱。留 `Raw()` 作为逃生舱并标注风险。
3. **不抹平错误。** 断开原因必须能被上层区分，否则会退化成"连接断了"这一种情况。
4. **不藏读循环。** 背压是业务策略，封装只提供正确的挂载点。
5. **策略可配，行为不可配。** 超时时长是配置；"写必须加锁"不是。
6. **server / client 共享底层，不强行统一上层。** `Upgrader` 和 `Dialer` 的关注点差别很大。

---

## 3. 分层

```
L3  接入层     Dial() / Upgrade()          ── 各自独立，产出 *Conn
L2  语义层     Router / Expect-Wait        ── 事件路由、请求-响应关联（可选）
L1  生命周期   Serve() / Disconnect        ── 心跳、超时、读循环、panic 防护、归因
L0  连接层     Conn                        ── 线程安全写、幂等关闭、关闭归因
```

上层可以不用。只想要一个"写安全 + 有超时"的连接，用到 L0 就停。

---

## 4. 能力清单

### A 类：必备（缺了一定出 bug）

| # | 能力 | 说明 |
|---|---|---|
| A1 | 写串行化 | 所有写操作（含 `SetWriteDeadline`）在同一把锁内 |
| A2 | 写超时内置 | 每次写自动配 deadline，调用方无从遗漏 |
| A3 | 读超时 | 多久没有任何消息判定链路已死。防 TCP 半开永久阻塞 |
| A4 | 心跳 | 自动 ping + pong 续期读 deadline，与 A3 是一套 |
| A5 | 关闭归因 | 区分本端关闭 / 对端关闭 / 超时 / 协议错误，**不能只给一个 error** |
| A6 | 幂等关闭 | 任意 goroutine 任意次数调用 `Close` 都安全 |
| A7 | 读循环 panic 防护 | 一个 handler panic 不能打穿整条连接 |
| A8 | ReadLimit | 防超大帧打爆内存 |
| A9 | 收敛唯一性 | 三方都可能结束连接，但底层 Close 只执行一次、`OnDisconnect` 只调一次（§7.3） |
| A10 | 等待者唤醒 | 连接终止时所有 `Wait` 立即返回，不能挂到各自 ctx 超时（§7.4） |

### B 类：应有（不做会到处重复）

| # | 能力 | 说明 |
|---|---|---|
| B1 | 优雅关闭 | 发 close 帧 → 等对端回应（有上限）→ 兜底强关 |
| B2 | 事件路由 | 按 JSON 字段分发，字段名可配，未命中有 fallback |
| B3 | 请求-响应关联 | 发出请求后等待首个匹配响应，带超时 |
| B4 | 观测 hook | 连接数、连接时长、关闭原因分布、收发字节 |
| B5 | 日志注入 | 接口注入，不硬编码具体 logger |
| B6 | 时间闸 | `ActivateGrace` 防空连接占名额、`MaxLifetime` 防无限期持有资源（§7.6） |
| B7 | 连接注册表 | 并发上限（握手阶段拒绝）+ **等待收敛**的优雅退出（§7.7） |

### C 类：明确不做

| # | 不做 | 原因 |
|---|---|---|
| C1 | 自动重连（默认） | 对**有状态**协议是错的，见 §10 |
| C2 | 内建消息队列 / 背压策略 | 丢最老帧？阻塞？拒绝？是业务决策 |
| C3 | 消息重发 | 连接异常后流的位置已不确定，重发只会让数据错乱 |
| C4 | 统一 server / client 上层抽象 | 硬合并出来的接口两边都别扭 |
| C5 | 隐藏原始错误类型 | 上层判断需要它 |

---

## 5. API 设计

### 5.1 L0 —— Conn

```go
type Options struct {
    WriteTimeout time.Duration // 单次写超时，0 表示不限
    ReadTimeout  time.Duration // 多久没有任何消息判定死连接，0 表示不限
    PingInterval time.Duration // 心跳间隔，0 表示不发
    ReadLimit    int64         // 单帧上限，0 表示不限

    Logger Logger
    Hooks  Hooks
}

// Wrap 包装已建立的连接。
// 会校验 PingInterval <= ReadTimeout/2（见 §6.2）——配反了心跳就永远救不了
// 读超时，这种错误必须在启动时炸掉，而不是等线上连接莫名断开。
func Wrap(raw *websocket.Conn, opt Options) (*Conn, error)

// Send 发送一帧。内部完成「加锁 → SetWriteDeadline → 写」，全程持锁。
func (c *Conn) Send(mt int, data []byte) error
func (c *Conn) SendText(s string) error
func (c *Conn) SendBinary(b []byte) error
func (c *Conn) SendJSON(v any) error

// Close 幂等关闭：发关闭帧并标记「本端主动」，随后关闭底层连接。
func (c *Conn) Close(code int, reason string) error

// ClosedByUs 报告连接是否由本端关闭。
// 读循环在本端关闭后必然报错，这个标志用来把它和「对端断开」区分开。
func (c *Conn) ClosedByUs() bool

// Raw 逃生舱。使用前请确认你清楚 gorilla 的并发约束——
// 绕过 Send 直接写会破坏写串行化。
func (c *Conn) Raw() *websocket.Conn
```

**刻意不导出**：`SetWriteDeadline`、`SetReadDeadline`、`WriteMessage`。
前两者由 `Options` 接管，后者由 `Send` 取代。这是本设计最重要的一个取舍——
把"每次写前记得设 deadline，并且要和写在同一把锁里"这条约定，从注释变成结构保证。

### 5.2 L1 —— Serve 与 Disconnect

```go
type Cause int

const (
    CauseLocal    Cause = iota // 本端主动关闭（含 ctx 取消）
    CausePeer                  // 对端发来关闭帧
    CauseTimeout               // 读超时，链路已死
    CauseProtocol              // 帧格式 / 超限 / 解析失败
    CauseFatal                 // 其余 I/O 错误
)

type Disconnect struct {
    Cause  Cause
    Code   int    // WebSocket 关闭码；无关闭帧时为 1006
    Reason string
    Err    error
}

type Handler interface {
    OnOpen(*Conn)
    OnMessage(c *Conn, messageType int, data []byte)
    OnDisconnect(c *Conn, d Disconnect)
}

// Serve 阻塞运行直到连接结束，返回断开归因。
// 内部负责：心跳 ticker、pong 续期读 deadline、读循环、逐条消息 recover。
// ctx 取消时主动优雅关闭，归因为 CauseLocal。
func (c *Conn) Serve(ctx context.Context, h Handler) Disconnect
```

三个关键决定：

**返回 `Disconnect` 而不是 `error`。** 强迫调用方面对"为什么断的"。
`error` 太容易被 `if err != nil { log }` 一笔带过，而不同 `Cause` 的处理天差地别。

**`Cause` 的判定依据不是关闭码。** 对端发 `1000` 正常关闭帧，从传输层看是"正常"，
但如果业务约定的终结消息还没到，那就是**异常终止**。所以：

- `CauseLocal` 只由 `ClosedByUs()` 决定
- `CausePeer` 只说明"对端关的"，**不判断是否正常**——正常与否是业务语义，由上层结合协议状态判断

按关闭码放行是个很隐蔽的坑：它会让对端静默退出变成"看起来一切正常"，上层什么都收不到。

**`OnDisconnect` 保证被调用一次，含 panic 路径。** 有了这条保证，上层才能安全地把
"必须给出一个结局"的逻辑挂在这里。

### 5.3 L2 —— Router 与 Expect/Wait

```go
type Router struct {
    EventKey string // 路由字段名，默认 "event"
}

func (r *Router) On(event string, fn func(*Conn, []byte))
func (r *Router) Off(events ...string)

// Handler 适配：把 Router 作为 Serve 的 Handler 使用，未命中走 fallback。
func (r *Router) Handler(fallback func(*Conn, int, []byte)) Handler
```

请求-响应关联做成**两段式**，而不是一个 `Await(ctx, event)` 函数：

```go
w := router.Expect("task-started", "task-failed") // 1. 先注册等待
defer w.Cancel()
conn.SendJSON(runTask)                            // 2. 再发请求
name, data, err := w.Wait(ctx)                    // 3. 后等响应
```

两段式是为了消除一个真实的竞态：**响应可能在你开始等之前就已经到达**。
单函数 API 会诱导人写成"先发再 Await"，看起来自然，但中间存在丢消息的窗口。
API 形状本身就该把正确顺序固定下来。

`Wait` 的语义：命中任一注册事件即返回并自动注销；`ctx` 超时返回错误；
连接在此期间断开也要立即返回（不能等到 ctx 超时才醒）。

### 5.4 L3 —— 接入层

```go
// 客户端
func Dial(ctx context.Context, url string, opt DialOptions) (*Conn, error)

// 服务端
func Upgrade(w http.ResponseWriter, r *http.Request, opt UpgradeOptions) (*Conn, error)
```

两边**不共用配置结构**：`DialOptions` 关心代理、TLS、握手头、子协议；
`UpgradeOptions` 关心 Origin 校验、缓冲区、子协议协商。硬合并只会得到一个两边都有一半字段没用的结构。

`UpgradeOptions.CheckOrigin` **默认拒绝跨域**，白名单显式配置。
gorilla 默认放行所有来源，是个常见的安全脚印，封装层应该把默认值扳回安全的一侧。

### 5.5 观测

```go
type Hooks struct {
    OnConnect    func(info ConnInfo)
    OnDisconnect func(info ConnInfo, d Disconnect)
    OnFrame      func(info ConnInfo, messageType int, size int, out bool)
}
```

建议落的指标：活跃连接数、连接时长直方图、**按 `Cause` 分组的断开计数**、
收发字节数、心跳失败数。其中按 `Cause` 分组是最有价值的一个——
`CauseTimeout` 突增说明网络或对端有问题，`CauseProtocol` 突增说明有客户端版本不兼容，
这两件事混在一个"连接断开"计数里就什么都看不出来。

---

## 6. 探活设计

### 6.1 「活」有三层，ping/pong 只能证明中间那层

| 层次 | 含义 | 探测手段 | 能证明什么 |
|---|---|---|---|
| 传输层 | TCP 连接还在 | TCP keepalive | 几乎无用（默认 2 小时） |
| 协议层 | 对端 WebSocket 栈还在响应 | ping / pong | 网络通、进程活着 |
| 业务层 | 对端还在正常推进协议 | 业务消息 / 业务心跳 | 对端**真的在干活** |

关键区别：**对端进程假死时 pong 照样回**。GC 长停顿、业务线程死锁、上游服务内部故障但网络栈正常——
这些情况下协议层探活全部通过，业务却已经停了。

所以探活要有**两条独立通道**：

- **协议级**：ping / pong，由封装自动完成
- **业务级**：读超时（任何入站帧都算续期），封装提供机制，**阈值和"什么算业务活"由应用定**

第二条是很多封装漏掉的。做法上不需要额外通道——只要把"读 deadline 由任何入站帧续期"这条规则定死，
数据帧、业务心跳、pong 就自然统一成同一个信号。应用要做的只是保证协议本身在静默期有东西可发
（很多上游服务有可选的 heartbeat 参数，**要主动开启**，否则读超时会误杀正常的长静默连接）。

### 6.2 参数关系必须在构造时校验

```
PingInterval <= ReadTimeout / 2
```

为什么是 `/2` 而不是简单的小于：**要容忍丢一次 pong**。

gorilla 官方 example 用的是 `pingPeriod = pongWait * 9 / 10`（54s / 60s），
那个比例只保证"在 deadline 到期前把下一个 ping 发出去"，**一次 pong 丢失就会误判**。
取 `ReadTimeout / 2` 则保证 deadline 窗口内一定有两次 ping 机会，抗一次丢失。

配反了的后果是心跳永远救不了读超时，连接会周期性地莫名断开——
这种错误必须在 `Wrap()` 时直接返回 error，不能等线上暴露。

### 6.3 写超时是最快的死连接探测器

容易被忽略：**写失败通常比读超时更早发现问题**。对端不再 ACK 时，
TCP 发送缓冲区填满 → 写阻塞 → `WriteTimeout` 触发，这比等满一个 `ReadTimeout` 周期快得多。

因此：

- 写超时失败 → **直接判定连接死亡**，不重试写
- 心跳 ping 的写失败 → 立即判死，不用等读超时

重试写是错的：连接状态已不确定，重发会让帧序错乱（见 C3）。

### 6.4 不同流量模式统一处理

| 模式 | 探活主力 |
|---|---|
| 持续数据流（音频、行情） | 数据帧本身，ping 是冗余备份 |
| 长静默（订阅推送） | 完全依赖 ping / pong |
| 单向流（只上行或只下行） | 静默那一侧必须靠 ping |

一条规则覆盖三种：**读 deadline 由任何入站帧续期，ping 独立定时发送**。

### 6.5 一个阈值就够，不要多计数器

有种设计是"连续 N 次 ping 未收到 pong 就判死"。不推荐——
pong 本身也是入站帧、也会续期读 deadline，所以"N 次 ping 失败"和"读超时"本质是同一个信号，
维护两套计数器只会增加出错面。

**唯一的例外是 ping 的写失败**（§6.3），那是独立信号，值得立即判死。

---

## 7. 连接生命周期

### 7.1 状态机

```
   Dial / Upgrade
         │
         ├──失败──► Failed
         ▼
     Active ─────────────┐
         │               │ ReadTimeout / IO 错误
         │ Close() /     │ MaxLifetime 到期
         │ 收到 close 帧  │
         ▼               │
     Draining            │
         │ 对端回应 or    │
         │ CloseTimeout   │
         ▼               ▼
       Closed ◄──────────┘
         │
         ▼
   OnDisconnect（保证恰好一次）
```

`Draining` 是最容易漏的状态：发出 close 帧后**不能立即关 TCP**，要给对端回应的机会；
但也**不能无限等**，所以要有 `CloseTimeout`（建议 2–5s）。

### 7.2 优雅关闭的完整序列

**主动关闭方**：

1. 停止发送新的业务消息
2. 发 close 帧（带 code + reason）
3. 等对端回 close 帧，上限 `CloseTimeout`
4. 无论是否等到，关闭底层 TCP
5. `OnDisconnect(CauseLocal)`

**被动关闭方**：

1. gorilla 的**默认 CloseHandler 会自动回一个 close 帧**——这点很多人不知道，
   自己再回一次是多余的
2. `ReadMessage` 返回 `*CloseError`
3. 关闭底层 TCP
4. `OnDisconnect(CausePeer, code, reason)`

### 7.3 三方都可能结束连接，但只能收敛一次

连接可能被三方结束：应用主动、对端主动、传输故障。设计必须保证：

> **无论哪条路径，底层 Close 只执行一次，`OnDisconnect` 只调用一次，含 panic 路径。**

用 `sync.Once` + 原子状态实现。这条是整个生命周期管理的地基——
有了它，上层才能安全地把"必须给出一个结局"的逻辑挂在 `OnDisconnect` 上。

### 7.4 资源回收清单

连接结束时必须释放：

- ping ticker
- 读循环 goroutine（`Serve` 返回即结束）
- **所有等待中的 `Expect`/`Wait`——必须被立即唤醒**
- 应用侧的队列、上游连接等（通过 `OnDisconnect` 通知）

第三条单独强调：等待者如果只监听自己的 ctx 超时，连接断开后它们会**继续挂到超时才醒**，
资源迟迟不释放，日志上还会看到一堆莫名其妙的超时错误。所有 `Wait` 必须同时监听连接终止信号。

### 7.5 与 context 的绑定

```go
func (c *Conn) Serve(ctx context.Context, h Handler) Disconnect
```

ctx 取消 → 触发优雅关闭 → 归因 `CauseLocal`。

**这里有个高危陷阱**：绝不能传 HTTP 请求的 ctx。连接升级后请求已经被 Hijack，
请求 ctx 在语义上已不适用，但它仍然会按 HTTP 框架配置的超时被取消——
结果就是 WebSocket 连接在固定秒数后被无故关闭，且现象极难定位。

封装层可以做个防呆：**`Serve` 检测到传入的 ctx 带 deadline 时打一条 warn 日志**。
长连接的 ctx 正常情况下不该有 deadline，有就说明大概率传错了。

### 7.6 资源治理：两道时间闸

心跳会无限期地把连接续着，所以必须有独立于探活的时间上限，否则一个只连接不干活的客户端
就能永久占住资源：

```go
type Options struct {
    // ActivateGrace 建立后多久内必须 MarkActive，超时关闭。
    // 防「只连不用」的空连接占用名额。
    ActivateGrace time.Duration
    // MaxLifetime 单连接最长存活时间，到点强制关闭。
    // 防长连接无限期持有上游资源（含持续计费的场景）。
    MaxLifetime time.Duration
}

// MarkActive 由业务在连接进入正常工作状态时调用。
// 「什么算工作状态」是业务定义（收到首个指令、完成鉴权、上游就绪等），
// 封装只提供闸门，不猜语义。
func (c *Conn) MarkActive()
```

到闸时的行为：**先发一条业务可理解的错误消息，再关闭**。
静默断开会让对端以为是网络问题，排查成本极高。

### 7.7 服务端全局治理

单连接之外还需要一个进程级组件：

```go
type Registry struct{ ... }

// Acquire 申请一个并发名额，占满返回 false。
// 必须在**升级之前**调用——满了就直接返回 HTTP 503，不要升级完再关。
func (r *Registry) Acquire() (release func(), ok bool)

func (r *Registry) Track(c *Conn)
func (r *Registry) ActiveCount() int

// CloseAll 广播关闭并**等待**收敛，ctx 控制等待上限。
func (r *Registry) CloseAll(ctx context.Context) error
```

两个细节：

- **名额在握手阶段就要判断。** 升级成功后再关连接，对端会先看到连接成功再看到断开，
  比直接拿到 503 难排查得多。
- **`CloseAll` 必须等待收敛，不能只发信号。** 进程退出时如果不等，
  正在收尾的连接可能来不及把最后的业务消息和 close 帧发出去。
  名额的释放时机同理——要等连接真正结束，否则会出现短暂的超发窗口。

---

## 8. 并发模型

每条连接固定 2 个 goroutine：

| goroutine | 职责 |
|---|---|
| `Serve`（调用方） | 读循环 + 分发。**唯一的读者** |
| ping ticker | 定时心跳。写安全，因为 `Send` 持锁 |

写没有专属 goroutine——`Send` 内部加锁，任意 goroutine 都能安全调用。

**必须写进文档的一条约定**：`OnMessage` 运行在读循环 goroutine 上。
handler 里做耗时操作会阻塞整条连接的读取；要投递到别的 goroutine，
同步责任在应用侧。封装不代管，因为队列长度、满了之后丢弃还是阻塞，都是业务决策（C2，见 §9）。

---

## 9. 不封装背压，但要给对位置

有界队列 + 满了丢最老，是流式音频这类场景的典型策略；
但对控制指令类连接，丢消息是不可接受的。所以封装只保证 `OnMessage` 是唯一的挂载点，
策略由应用在 handler 里实现：

```go
func (h *myHandler) OnMessage(c *wsx.Conn, mt int, data []byte) {
    select {
    case h.ch <- data:
    default:
        h.dropOldest(data) // 或阻塞、或断开——业务自己定
    }
}
```

配套要求：**任何有界队列都必须暴露丢弃计数**，否则丢帧变成静默的数据损坏。

---

## 10. 关于自动重连

**默认不提供，且不建议对有状态协议开启。**

自动重连隐含一个假设：连接是无状态的，断了重连就恢复原状。这个假设在很多场景不成立。
以流式语音识别为例，重连意味着重新开一个识别任务，代价是：

- 序号从头开始，上层若按序号索引会**覆盖**已有数据
- 断开瞬间正在处理的那部分永久丢失
- 计费 / 用量统计被切成多段
- 重连期间的输入要么丢弃，要么堆积

这些都不是"重试"能解决的，而是需要上层显式做状态映射。**把它藏在封装里，等于把数据正确性问题变成了看不见的问题。**

如果确实需要（无状态的订阅推送、行情流等），做成显式策略：

```go
type ReconnectPolicy struct {
    MaxAttempts int
    Backoff     func(attempt int) time.Duration
    // ShouldRetry 由调用方判断：鉴权失败、参数错误一律不该重试。
    ShouldRetry func(d Disconnect) bool
}
```

关键是 `ShouldRetry` 必须由调用方提供，**没有合理的默认值**——
默认重试会在对端已经故障时持续加压。

---

## 11. 验证清单

设计是否够用，用真实踩过的坑回归验证：

| 曾经的坑 | 本设计如何覆盖 |
|---|---|
| 并发写导致帧错乱 | A1：`Send` 全程持锁 |
| `SetWriteDeadline` 不在写锁保护内 | 不导出，由 `Options.WriteTimeout` 接管 |
| TCP 半开时读循环永久阻塞 | A3 + A4，构造时强制 `PingInterval <= ReadTimeout/2` |
| 对端发 `1000` 关闭帧后静默退出，上层什么都收不到 | A5：`Cause` 由 `ClosedByUs()` 判定，不看关闭码 |
| 事件流结束却没给出终结事件 | `OnDisconnect` 保证被调用，含 panic 路径 |
| handler panic 打穿整条连接 | A7：逐条消息 recover |
| 响应先于等待到达而丢失 | B3 的两段式 `Expect` / `Wait` |
| 跨 goroutine 投递事件与关闭 channel 竞态 | **封装管不了**，属应用层。§6 明确写清 `OnMessage` 的 goroutine 归属 |

最后一行同样重要：**说清楚边界在哪，比多封一层更有价值。**

---

## 12. 目录结构

```
pkg/wsx/
  conn.go       L0：Conn、Options、写串行化、关闭归因
  serve.go      L1：Serve、Disconnect、心跳、读循环
  router.go     L2：Router、Expect / Wait
  dial.go       L3：客户端接入
  upgrade.go    L3：服务端接入
  hooks.go      观测
  logger.go     日志接口
```

---

## 13. 实施顺序

1. **L0 + L1**（`conn.go` / `serve.go`）——价值最高，A 类能力全在这里。
   验收：并发写压测无 panic；模拟半开连接能在 `ReadTimeout` 内判死；
   对端发 `1000` 关闭帧能归因为 `CausePeer` 而非"正常"。
2. **L3 接入层**——薄，跟着 L0 一起出。
3. **L2 语义层**——只有出现第二个使用方时才做。一个使用方的抽象是猜的。

先在一个真实场景上跑通，再考虑推广。
