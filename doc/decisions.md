# 设计决策记录

这里记录 egop 里几处**有意为之**的取舍——它们看起来像「缺口」，其实是边界/语义的显式选择。贡献者与使用者在改动前请先读这里，避免把决策当 bug 改回去。

## 1. 事件扇出是同步的，不是异步队列

`MemEvents.Dispatch` 会阻塞到所有订阅者回调返回。不引入缓冲队列/背压/丢弃策略，因为插件回调通常是轻量的状态更新，同步语义更可推理、也没有 goroutine 泄漏与顺序问题。要异步解耦，由装配层的 `Events` 实现自行选择。

## 2. 投递的事件是共享只读的，不深拷贝

同一 `Event` 值扇出给多个订阅者，`Labels`（map）与 `Source`（指针）是引用共享。订阅者必须按只读使用，不得改写（否则污染其它订阅者与发布者）。深拷贝 per-subscriber 性价比低，故只文档约定、不实现。

## 3. 事件过滤只匹配结构化字段，不解析 Payload

`EventFilter` 匹配 `Type`（通配）/`SubType`/`SourceID`/`SourceVersion`/`SourceKind`/`Labels`。
`Payload` 是不透明 JSON，不参与匹配——一旦要「按 payload 内部字段 match」就变成 JSONPath/查询引擎，
破坏「内容无关、零解析」这条立身之本；更深的匹配留给订阅方在回调里自己解。

## 4. `ctx` 不跨边界（远程/wasm 无 ctx）

`context.Context` 是进程内活体（deadline/取消通道/value 树），无法经 wire/ABI 序列化。
能过边界的是**数据**（Event JSON），不是 ctx。所以远程插件回调的 `ctx` 是**本侧会话上下文**、
wasm guest 无 ctx——事件里真正有用的存根（`Source`/`Labels`/`Payload`）都在 `Event` 本体上。

## 5. `egop_init`/`egop_shutdown` 用固定超时，不走 ctx

`Surface.SetSurface` 无 ctx（`Register` 链也无 ctx），`egop_init` 用 `initTimeout=30s` 固定兜底；
`egop_shutdown` 在 `Close(ctx)` 里再包 `shutdownTimeout=10s` 上限。wasm guest 是纯计算
（无 fs/网络注入），固定超时已足够接住死循环；为「调用方可控 deadline」去动整条
`Register`/`SetSurface` 注册链是 breaking 且收益低。

## 6. 运行时事件 topic 不自动命名空间

声明侧有 `EventID`/`PointID`（`dyn.<plugin>.<topic>` 去冲突），但运行时 `Publish(ctx, topic, …)`
是自由裸字符串，不自动加插件前缀。这是「任意主题」语义的代价；跨插件同名主题需插件作者
自觉用 `EventID` 命名。强制前缀化会抹掉自由主题能力，故维持约定。

## 7. `Host.Remove`/`Replace` 不调用 `Disposer`

`Disposer.Close` 仅 `Host.Close` 统一调用；单件 `Remove`/`Replace` 只清目录与 effect 栈，
**不** dispose 原生资源——调用方须自行 `Close` 插件句柄（`autoload` 正是如此：卸载/热替换后
立即 `plugin.Close`）。这样 `Remove`/`Replace` 无需 ctx，优雅关停的 ctx 也仍由调用方掌握。

## 8. `Surface.GetSetting` 无能力门控

能力词清单里没有 settings 读词：settings 是只读的宿主共享环境值，无隔离诉求，故不做
「先说后做」。与 `plugin.meta`/`config.read`/`net.access` 等受门控的能力不同，这是刻意简化。

## 9. 能力门控与八轴求差是「最小契约」

`SlotSpec` 逐轴求差是「只多不少」的**最小**契约（声明面 ⊇ 槽位要求），不是相等校验。
插件可以比槽位要求提供更多。

## 10. 配置的双向契约与「读回真值」

配置契约是成对的：`Configurable.ApplyConfig`（写，host→插件）+ `ConfigProvider.Config`
（读，插件→host）。读侧**优先级**是 `ConfigProvider.Config()`（插件自述的权威生效配置，
含默认值补齐/归一化/脱敏）→ 回退 `host.applied`（宿主推过的原始 delta）——因为缓存记的是
「推了什么」，不等于「插件实际跑在什么配置上」。`ConfigFieldSpec.Default` 是**声明级默认值**
（供 web UI 展示/回填），运行时真实默认仍以插件 `Config()` 为准。写侧语义显式区分：
`Host.SetConfig` = 整对象替换，`Host.SetConfigField`（及门控的 `Surface.SetConfig`）= 单字段合并。
## 11. wasm 事件/hook 投递不重入取锁（TryLock，锁忙即跳过）

决策 1 的同步扇出保留，但 wasm 侧 `pushEvent`/`invokeHook` 用 `TryLock` 而非阻塞取锁。
原因：同步扇出会在**发布者自己的 goroutine** 上执行订阅回调——「guest 调用中发布命中
自身订阅的事件」即同 goroutine 重入实例锁，对非重入的 `sync.Mutex` 就是永久死锁
（有 `testdata/selfpub.wat` 夹具回归固化）。备选的「每插件异步投递队列」保投递但引入
goroutine 生命周期/背压/顺序问题，且只对 wasm 世界有意义（进程内/远程插件不持实例锁），
收益与复杂度不成比例——事件本就是 best-effort 观察面，丢弃一致于「投递失败静默」。
代价（显式接受）：实例忙时**并发**发布的事件也会被丢弃而非排队；hook 锁忙时返回
非阻断 `HookResult{Reason:"instance busy"}`，触发链不断。

## 12. 槽位依赖的卸载判定：按「删除后槽位是否仍被满足」

`Remove`/`Dependents` 对 `DepInit{Slot}` 依赖不做「被删插件曾占据该槽位即连坐」的
过度拒绝，而是精确判定：删除后槽位仍有其它在册主张者（或内置）→ 依赖未破坏。
只有删**唯一供给者**才 fail-closed/级联。槽位名与插件 id 是两个命名空间，判定必须拿
被删插件的 `Meta.Slot` 去比（曾经直接 `r.Slot == pluginID` 比较，是判不出的 bug）。

## 13. `Replace` 与 `Register` 同款契约校验

热替换口不得比注册口宽松：`DepInit` 依赖就位与槽位八轴最小契约在 `Replace` 里同样
强制（共用 `validateContractLocked`）。校验失败拒换、旧版原样在册——与热更「替换失败
回退保旧版」语义同构。曾经 `Replace` 零校验，坏包可从热更口静默绕过契约。

## 14. 删除也是两段确认；mount 装配失败全清

与增/改的两段确认对称：文件**连续两轮未见**才卸载——目录瞬态读失败、文件系统抖动
不再一轮误卸全部插件（曾经 WalkDir 失败 → seen 为空 → 全量立即卸载）。代价：卸载
延迟多一个轮询周期。`mount.Mount` 失败路径（出站拨号/注册失败）额外做全清：
`Watcher.Unload` 反注册并**关闭句柄**（wazero runtime 不泄漏），宿主回到装配前状态；
正常关停（`Runtime.Close`）不做反注册——注册面归宿主总闸（`Host.Close`），避免双重
清退的所有权混乱。

## 15. 插件包是不可信输入：zip 上限与后缀注入

zip 解压设单条目/聚合双上限（默认 256MiB/1GiB，头部声明预检 + 读取硬限，谎报尺寸
也兜得住），资产名拒绝绝对路径与 `..` 穿越（zip-slip 防在源头，消费方拿 `Assets()`
落盘服务也安全）。包后缀只内置 `.egop.wasm`/`.egop.zip`：品牌/项目自有后缀经
`Options.ExtraSuffixes` 装配注入（内容无关库不内置业务词），判定收敛在
`wasm.IsPluginFile` 单点——扫描侧与装载侧永不分叉。

## 16. Meta 注册快照:入册即深拷贝(CloneMeta)

宿主在 Register/Replace 入册时经 `contract.CloneMeta` 深拷贝 Meta(全部
slice/map/RawMessage 字节)。原因:`h.meta[id]=m` 值拷贝只拷壳,Provides/Requires
的引用部件与插件侧共享——插件注册后改写自己的 Meta(或并发读写)会让宿主的
八轴校验/Dependents/控制面视图静默漂移,还构成数据竞争面。快照代价(注册路径
低频、字段量小)可忽略;宿主外发(Plugins/Snapshot)共享同一份冻结拷贝也安全。

## 17. 扩展键值铺满全部声明结构 + `egop.` 保留前缀

`Extensions map[string]json.RawMessage`(即 JSON 世界的 (key string, value any))
从 Meta/FuncSpec 铺满到 HookPointSpec/EventTopicSpec/ConfigFieldSpec/Dependency/
SlotSpec——「没被固定字段覆盖的自定义含义」在每一层声明上都有同一形状的口子,
取值助手 `contract.Ext[T]`。键空间保留前缀 `egop.`(ReservedExtPrefix)给 egop
自身特性(在用:egop.pool):开发者自定义键不用它,未来 egop 新键不与用户撞名。
egop 此前在 `egop.pool` 上开了库读用户可见扩展键的先例却无保留约定,是潜在撞名源。

## 18. 四个"声明了但零接线"的轴,接线或删除

契约里声明了语义但机制层零消费的词汇一律处置(声明面在说谎是最差状态):
- `ConfigFieldSpec.Secret`:接进 SetConfig 观察事件脱敏,**声明优先**——顶层命中
  即遮(键名不敏感也遮),键名子串启发(token/secret/…)降为兜底。声明真源优于启发。
- `HookPointSpec.Kind`(observe):接进 TriggerHook——observe 点回调的 Block 按
  声明丢弃(Reason 注明);modify(缺省)不限制。声明表(hookDecls)注册/替换/删除
  后全量重建,注册序=首声明者序(同名 hook 点多声明者并立是合法形状,hook 总线
  本就是平字符串命名空间)。
- `Meta.DependsOn`:删除。注释自认"遗留纯声明",全库零消费,死词只会误导
  (pre-1.0 直接 breaking,不走迁移文档)。
- `Provides.Events/Points/Listens`:保持**描述性**(发现/槽位契约轴),运行时
  发布仍按 event.emit 能力门控——但补上真正的完整性漏洞:**框架保留主题**
  (plugin.* 生命周期 + plugin.config.updated,contract.IsFrameworkTopic)插件经
  Surface 不可发布(单点拒发+留痕)。防插件伪造 plugin.removed 欺骗软依赖方。
  全面强制"发布仅限声明/自有 dyn.* 命名空间"被否决:会打死跨插件主题约定、
  需重编一批夹具,完整收益不成比例。
- `HookPointSpec.Desc`→`Description`(与其它结构统一拼写);`Dependency` 双空
  (Plugin/Slot 皆无)注册口拒载,双取 Slot 优先,未知 Kind 前向兼容忽略。

## 19. 函数目录键空间:函数名禁 `"."`,id 允许含点

`h.fns` 以 `id + "." + fname` 为键:函数名含点会让插件 `a` 的函数 `b.c` 与插件
`a.b` 的函数 `c` 撞键(表现为费解的 function conflicts)。裁决:**函数名禁 `"."`
与空白**(注册口单点 fail-closed),插件 id 保留含点(vendor.name 命名惯例合法,
撞键由函数名单侧封死)。同批:插件 id 拒空白/控制字符。

## 20. 能力词保持裸词,作用域参数不入契约轴

capabilities 维持 `[]string` 裸词(门控=布尔)。作用域(如 net.access 限域名、
fs.read 限目录)两处安放:策略在注入实现(Net/FS 后端自行收窄),插件想**自述
最小特权**写进 Extensions 按约定(后端读取并强制)。不给能力词加结构化参数轴:
会引入 per-能力校验语义,内容无关库解释不了任意业务作用域。

## 21. wasm 实例池:TryLock + ctx 重入标记,不用配额信号量

决策 11 的跳过语义保留给**事件/hook 投递**,但**调用链**的嵌套重入不能再丢
(eha 嵌套轮 live 事故):每实例一把锁,入口 TryLock 空闲实例——并发度由实例数
自然约束。同 goroutine 嵌套重入(宿主注入函数回调宿主、宿主再进同一插件)经
**ctx 重入标记**识别(acquire 成功后注入,经 wazero ctx 透传给宿主注入函数):
有其它空闲实例即取第二实例;池耗尽**立即**返回 busy 错误——此时等待即自死锁。
池大小由清单保留键 `egop.pool` 声明(缺省 1,上限 4;zip 与裸 wasm 都生效)。

曾试过"同容量信号量配额"制,在三处翻车后拆除(2026-09-16 修复):Close 置
nil 后 release `<-nil` 永挂(在册调用的 Host.Call 永不返回)、growPool 换 sem
破坏配额账、嵌套等待只认 ctx.Done 对 Background ctx 永挂。TryLock+标记在
语义不变的前提下无配额簿记可翻车。

**观察面钉主实例**:事件订阅与 hook 注册只挂 `insts[0]`,投递也只投主实例
——池>1 时每实例 init 各自订阅的"事件双投递/落点漂移"与 revive 累积 hook
注册(撤销挂错栈)都在此一并修正;次实例声明按无操作忽略。

## 22. revive:打断自愈 = 对单实例做一次热重启

ctx 取消看门狗/runtime trap 打断的实例不废插件:下次调用先重建(复用 compiled
不重编译)、`egop_init` 重放(guest 自挂订阅)、最近生效配置回放——内存态归零,
KV 等宿主侧持久态不受影响。显式 Close 是终态不复活(注销/关停与意外打断是两类
事件)。备份三锚(wasm 字节/装载选项/最近配置)留在 Plugin 上。

## 23. 活体工具面 egop_tool_specs:清单未录 ≠ 不存在

动态工具插件(如 MCP 客户端)的工具面是运行期发现的,线上清单只是快照。可选导出
`egop_tool_specs` 是活体真源:`ToolSpecs` 优先调它,无导出/失败/断后回落清单
`Manifest.Tools`(旧 guest 零影响);`ToolRaw` 对清单外名字再查一遍活体面。
注:这曾引入"宿主面(host.Tools 收集)在持 h.mu 下调插件代码"的同 goroutine
死锁面(plugins/call 注入都取 h.mu)——已修:收集方锁内只取 provider 快照、
锁外调(tools_lock_test 回归固化;AGENTS 已立不变量"持 h.mu 期间绝不调插件
代码")。

## 24. guest 两个保护:真系统时钟 + fs_read 尺寸护栏

- wazero ModuleConfig 缺省 Walltime/Nanotime/Nanosleep 是**假钟桩**(纪元 2022/
  ENOSYS):guest 内 time.Now/Since/Sleep 全部静默错乱(TTL/退避类插件必踩)。
  实例化一律接 `WithSysWalltime/WithSysNanotime/WithSysNanosleep`,clock.wat
  夹具固化防回退。
- wasm ABI `fs_read` 单次回传上限 8MiB:base64 信封放大 4/3 灌进 guest 64MiB
  线性内存前在宿主侧拒读(grep /tmp 撞 34MB 固件 OOM trap 实锤;symlink 预筛
  防不住)。护栏在读完之后校验——宿主侧缓冲完整文件的成本由注入实现自管。

## 25. autoload 失败件跨轮重试:失败也是进展

装载/注册失败的文件**保留观察槽**且 pending=当前 hash,下一轮 Poll 直接重试——
依赖链乱序(DepInit 目标后落地)时先失败件必须能跨轮补载(旧语义 delete 观察槽,
zip-only 装配实锤下 subagents 永不落载)。mount 首装把 ActionFailed 也算"进展"
(否则拍稳停轮会把重试中的插件永久落下)。代价(显式接受):永久坏件每轮重试、
每轮一条 failed 事件;mount 首装上限 maxInitRounds=64 兜底。已知待优化:重试是
整 LoadFS(全量重编译),应区分"内容坏(等 hash 变)"与"依赖未就绪(保住已加载
实例只重试 Register)"。

## 26. 远程通道入站请求并发派发(有界),不再内联

recvLoop 曾把入站请求帧(CallFunc/Tool/Hook/ApplyConfig/HostCall)**内联同步**处理:
一个慢 op 队头阻塞整条会话(其它请求的回复路由、事件推送全部排队),同会话自调
(插件经 HostCall 回程调回自己)更是永久死锁——嵌套请求帧只有这条忙着的读循环能读。
裁决:请求类帧派发到**带界工作 goroutine**(配额制,满即回执 busy 背压,
绝不阻塞读循环;上限默认 `DefaultDispatchConcurrency=1024`——量级取"合法高并发
(慢处理器)永远碰不到、只有对端病态洪水才碰",32 的初值对热点慢处理器插件过紧,
经 DialOptions/WithDispatchConcurrency/mount RemoteSpec 按需覆盖,有
TestDispatchConcurrencyLimit 固化"满即 busy 不等待、释放即恢复");回复路由、Subscribe/Ping、push_event 保持内联(事件投递保序,
慢处理器是插件自己的事)。代价(显式接受):入站请求的**处理不再保序**(帧按序
读入、回复按 id 关联,顺序无依赖);插件侧 PluginOps 回调可能**并发**——与进程
内插件 CallFunc 的既有并发语义对齐(作者侧回调须线程安全)。配套:响应 body
句柄(per-handle 锁)串行化同句柄的读/关——并发派发后 io.Reader 撕裂是真实风险。
回归:TestSameSessionNestedSelfCall(旧形状直接死锁,测试超时)。

## 27. 投递看门狗 + wasm 字段竞争收口

- pushEvent/invokeHook 直调 fn.Call 无看门狗:挂死的 guest on_event/on_hook 会
  永挂总线同步扇出的 goroutine(发布方实例锁永不释放)。callExport 的看门狗机制
  抽成 callCore 共享:投递用 **10s 兜底时限 + WithoutCancel**——发布者/触发方
  ctx 的取消不传导(投递是 fire-and-forget,接受即送达或超时打断),只有时限
  经 CloseWithExitCode 打断(broken→下次调用 revive)。egop_on_event 走 callVoid
  (无返回值形态),egop_on_hook 走 callExport(信封复用)。
- 竞争收口:Close↔revive、ApplyConfig↔revive 对 p.compiled/p.lastCfg 的读写
  经 poolMu 快照;instantiateOne 显式收 compiled 参数;p.surface 改原子指针
  (宿主注入热路径无锁读,SetSurface 单写)。

## 28. autoload 失败重试分层(落地 #25 记的待优化)

#25 的"失败件跨轮重试"代价是每轮全量重编译(永久坏件 mount 空转 64 轮、watch
每秒编译一次)。分层修正:内容坏(LoadFS 失败/契约拒载)记 errHash——**hash
未变不重试**(等真变化,重新两段确认再试);注册瞬态失败(依赖未就位)**保留
已装载实例 np**,后续轮次只重试 Register(不重编译;依赖到位后注册的正是保留
实例)。mount 首装自然收敛(内容坏件不再每轮产出 ActionFailed)。配套:mount
的 accept/ServeStream goroutine 计入 WaitGroup,Close 释锁后 join——关停不留
迟到注册窗口。回归:TestFailedContentNotRetriedUntilChange、
TestRegisterFailureKeepsInstance。
