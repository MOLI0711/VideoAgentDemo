# 开发实施状态 · 2026-10-23

依据技术设计与开发文档，在本地 `feat/video-agent-platform` 工作区落地首版；10 月 23 日继续改为 vedioagent 免密码开放工作台并补齐本地依赖。发布目标为 [VideoAgentDemo](https://github.com/MOLI0711/VideoAgentDemo) 的 `main` 分支。原设计文档保留，本文记录实际结果，不将计划项当作已完成。

## 已实现

| 范围 | 实现与验证 |
|---|---|
| Go 应用 | api、media worker、agent worker、coordinator、devtool；配置校验、显式密钥环境变量、顺序数据库迁移 |
| HTTP/安全 | 独立访客身份、无密码 Cookie 会话、CSRF、Origin 校验、逐资源 owner 过滤、短时预签名、Redis 可选限流、40 个实际 HTTP 操作的 OpenAPI |
| 上传 | 8 MiB 分片、3 并发、断点清单/指纹、分片 ETag/尺寸校验、完成响应丢失恢复、完成幂等 |
| 视频处理 | PROBE → 音频/抽帧 → ASR/OCR → BUILD_SEGMENTS → BUILD_INDEX；无声分支、OCR 降级、真实时间偏移、attempt 产物隔离 |
| 持久任务 | MySQL DAG、Outbox/Inbox、租约与心跳、deadline、失败重试、取消、旧 lease fencing、publisher attempt 与恢复 |
| Kafka | broker ack 后标记、Inbox 接管后提交 offset、重复事件去重、毒消息隔离、数据库失败后重放 |
| 检索 | ES BM25 + dense vector + RRF，owner/video/content/build 四重过滤，维度校验、bulk 部分失败不发布、逆序 generation 发布保护 |
| Agent | 草稿/发布/停用版本，固定版本会话；实际 Eino ADK；搜索、读取片段、画面检查、已有概要四个工具；引用 ID 校验、完整工具消息恢复 |
| 摘要 | 独立 Map/Reduce；已完成分组与归并结果持久化，恢复不重复调用；最终 checkpoint 可直接完成 |
| 预算 | 跨 attempt 的模型/工具/视觉次数；文本调用预留与结算账本，未知保留预留、已知幂等结算、超估计实际用量如实记录 |
| SSE | 持久 seq、快照、after_seq/Last-Event-ID 恢复、终态事件与结果同事务 |
| 前端 | 自动访客空间、视频库、上传、任务进度、播放器与引用定位、会话/回答、Agent 版本管理、服务 disabled/mock 状态提示 |
| 工程资料 | 示例 YAML、Compose、worker 镜像定义、Makefile、CI 配置、OpenAPI 3.1、评测脚本、固定检索 baseline |

文本预算采用“请求编码字节数 + 最大输出 tokens”的保守估计，QA 上限 400000、摘要 1500000 预算单位，首笔预留后固定到任务。供应商返回已知 usage 时结算实际 tokens；未知不当作 0。该估计不能保证适配任意供应商 tokenizer，视觉与 embedding 没有纳入这一精确账本；视觉另受次数、帧数与图片大小限制。摘要另有 96 次调用上限。

## 首版验证记录（2026-10-22）

- Go 默认测试：88 个测试项通过，0 失败；3 个真实组件测试因默认未设置显式测试环境而跳过，下述专项已另行启用并通过。
- `go test -race ./...`、`go vet ./...` 通过；新增账本与恢复代码另通过真实 MySQL 回归。
- 前端 TypeScript、格式检查、生产构建通过；Vitest 3 个文件、14 项通过。
- OpenAPI：36 paths、40 operations、51 schemas、339 个本地引用，结构校验与代码路由覆盖通过。
- 真实 MySQL 8.4.6：并发幂等、配额锁、Outbox/Inbox、租约/取消/deadline、旧版本、越权、checkpoint、逆序发布、文本预算并发/结算通过。
- 真实 Kafka 3.9.0 + MySQL：Outbox → Kafka → Inbox READY，重复事件仍一条 Inbox，坏 schema 隔离与 offset 行为通过。
- 真实 SeaweedFS 3.85：预签名 multipart、分片列表、合并、逐字节读回、ETag/CORS、匿名读取拒绝与 Abort 通过。
- 真实 FFmpeg 5.1.9 / Tesseract 5.3：合成非零起始时间视频，音频偏移、真实 PTS、时长修正、关键帧与 OCR 通过。
- 真实 Elasticsearch 8.17.0：四重范围过滤、维度不匹配、真实 bulk 部分失败不发布通过。向量来自明确 fixture，不代表真实 embedding 质量。
- Eino 工具循环使用实际 SDK + 明确的模型 fixture：调用、证据、已完成工具恢复、最终结果复用、预算耗尽通过。
- 摘要/账本真实 DB：map 中断、reduce 中断、final 恢复、95→96 次未知调用后拒绝继续；invalid JSON 仍结算已知用量、网络未知/非法用量/lease 失效不释放额度。
- 浏览器实际操作：demo 登录，创建 Agent、保存/发布/复制版本，19 MiB 视频三分片上传，任务取消与重试。
- Python 评测计分的两项边界测试通过；缺失人工标注与 usage 保持未知。

测试过程中修复了并发配额锁升级死锁、内容查询截断、旧 lease 归属污染、模型未知提交处理、真实媒体时间偏移、短片视觉证据关联、分片完成响应丢失等问题。没有调用付费模型或付费 ASR。

## 免密码版本（2026-10-23）

- 前端名称为 `vedioagent`；不再展示注册、密码或退出登录入口。
- `POST /v1/auth/guest` 自动创建独立访客 owner、30 天会话及已发布的“视频助手”；有效旧 Cookie 原样沿用，原有资料不会转为公共数据。
- `0003_guest_users.sql` 增加访客标识；密码登录 API 移除，devtool 不再生成演示密码或要求种子账号。
- 前端会话恢复为单次并发请求；身份变化不会重放旧写请求，也不会将旧身份响应显示到新空间。清除站点数据或访客会话到期后无法恢复原空间。
- 真实 MySQL 仓储和 API 回归通过：并发访客独立性、默认助手、事务回滚、双访客资源隔离、Origin/CSRF、IP 限流、旧 Cookie 保留。
- 前端 4 个测试文件、22 项测试、类型检查、格式检查、生产构建通过；OpenAPI 3.1 及 40 个实际路由对照通过。
- 浏览器真实复查：原会话无密码进入且保留资料；独立 localhost 新访客直接进入空资料库并获得已发布视频助手；自定义 API/Vite 端口代理连通。
- 真实上传合成视频后，经 Kafka/Outbox/Inbox 完成 PROBE、EXTRACT_AUDIO、EXTRACT_FRAMES；随后返回预期 `ASR_DISABLED`。没有将这一结果描述为完整智能解析成功。
- 新增 `make dev` 前台进程管理：编译、迁移、初始化桶、检查依赖后同时启动 API、两类 Worker、coordinator 和 Vite。Ctrl+C 停止本次启动的进程；底层容器与数据保留。
- `runtime-check` 实际执行媒体工具并验证 OCR 语言包、S3 桶和 Kafka broker，避免将仅存在的包装器误判为可运行依赖。

## 开发者本地复现入口（2026-10-23）

- 用户选择本地应用配合云 API；开发者配置自己的 API Key，不要求自建模型服务。访客仍无注册或密码入口。
- 新增 `deploy/local.py`、独立的 `deploy/compose.local.yaml` 与 `configs/container.example.yaml`。初始化创建 `.runtime/container/` 下的个人设置，不覆盖宿主机现有 `.runtime/env` 或 `configs/local.yaml`。
- 容器组合包含前端 Nginx、API、两类 Worker、coordinator、初始化任务和五类基础设施；后端镜像包含 FFmpeg/Tesseract，配置使用容器 DNS 名称。
- 默认关闭云服务，基础管理可运行；完整视频转写与问答仍需开发者的真实模型/ASR/COS 账号，不能把默认启动描述为已经提供真实 AI 服务。
- 已加入用户选定的 MIT 许可证和贡献指南，README 包含首次启动、个人 API 设置、修改后重建、停止保留数据与可选源码开发流程。
- 初始化、文件权限、环境隔离与日志脱敏 8 项 Python 测试通过，前端真实镜像构建、Nginx 配置和独立代理测试通过；包括 Cookie/Origin 透传、SPA、请求大小和 SSE 不缓冲。
- 容器内 CORS 探测改用内部存储地址，避免在初始化容器中错误访问宿主机 loopback；浏览器预签名地址保持原签名 host。
- 完整后端和前端镜像已真实构建，并在 Podman 5.8.7 / Compose provider 1.6、Linux amd64 容器环境启动；Docker Compose v2 为默认入口，但本轮没有在 Docker 引擎上重复验收。
- 使用独立端口和全新数据卷验证：自动迁移、初始化桶、两位独立访客、默认已发布助手、真实 S3 分片上传、CORS 和越权隔离通过；真实 Kafka 驱动媒体 Worker 完成 PROBE、EXTRACT_AUDIO、EXTRACT_FRAMES，随后按预期返回 `ASR_DISABLED`，没有调用云 API。
- 修复全新 Kafka 没有主题时任务等待的问题：非 production 的生产者与消费者允许 broker 自动创建主题；真实 Kafka/MySQL 集成测试不再预建主题，已验证自动创建、Outbox/Inbox、去重和毒消息隔离。production 仍需显式创建主题。
- 配置更新后重新 `up --no-build` 已实测：同一访客与已上传视频保留，修改的分片大小生效，五类中间件容器未重建；初始化检查及日志脱敏入口可用。本轮隔离验证不覆盖原有宿主机配置或数据。

## 当前本地环境

本地私有凭据保存在 `.runtime/env`，未提交、未打包。Makefile 优先采用存在的 `.runtime/dev.env`，否则采用 `.runtime/env`，也可用 `ENV_FILE=...` 显式指定。没有访客密码。

- 前端：`http://127.0.0.1:5173`；API：`http://127.0.0.1:8080`。
- MySQL `13306`、Redis `16379`、S3 `18333`、Kafka `19092`、Elasticsearch `19200`；基础设施只监听本机，尚未公网部署。
- FFmpeg/FFprobe/Tesseract 使用 `.runtime/media-bin/` 可执行包装器，由独立容器处理 `.runtime/media` 中的媒体文件；`chi_sim+eng` 已验证，容器不挂载仓库凭据。
- 基础设施恢复脚本：`sh .runtime/start-local-services.sh`，只恢复本任务的指定容器；当前 API、两类 worker、coordinator 已运行。
- 配置已启用 `redis.enabled=true`、`workflow.dispatch_mode=kafka`，应用需要 coordinator 与 media/agent 两类 worker。
- 用户确认尚无云账号，本轮先完成本地环境。`model.mode=disabled`、`asr.mode=disabled`、`search.enabled=false` 明确保留；ES 服务可以运行，但业务向量索引仍需真实 embedding 模型和正确维度。
- 可上传、管理视频和 Agent，执行本地媒体处理与持久任务。没有云 ASR 时语音步骤返回 `ASR_DISABLED`，不生成虚假字幕或答案。

本任务继续复用原有独立容器，没有删除其他项目资源。真实集成测试使用随机数据库、索引和 topic，仅清理自身测试数据。

## 尚未完成的验收与生产能力

1. 真实供应商 ASR、chat、tool calling、vision、embedding 的一体化连通与真实视频端到端验收，需要用户自己的凭据及合法视频。当前 provider 测试使用本地 HTTP fixture。
2. 20/100 条人工标注集、真实准确率/成本、并发压测、长视频生产参数没有数据结论。提供了工具，但不能将合成结果写入简历为真实业务指标。
3. 对象/旧索引/过期上传的自动 GC、保留策略后台和 `SUBMISSION_UNKNOWN` 人工核对恢复入口尚未实现；使用云存储前需配置适当生命周期。
4. 源视频统一播放转码未实现；浏览器无法播放的容器/编码会保留错误提示，不伪装播放成功。
5. 完整 OpenTelemetry 链路、成本看板、生产告警/访客配额与身份恢复策略、Kubernetes/公网发布尚未验收。CI 文件与 Dockerfile 已提供，容器镜像已在本机真实构建；尚未运行远端 CI，也未验收其他操作系统和 CPU 架构。

因此应描述为“已实现并验证核心工程链路的首版”，不能描述为“所有开发计划与生产验收均已完成”。下一步应先完成真实服务与视频验收，再决定性能、成本和规模指标。
