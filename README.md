# vedioagent · Go 视频 Agent 工作台

上传视频，异步提取音频、字幕、关键帧及 OCR，构建带版本的检索索引，再通过可配置 Agent 回答问题。回答引用字幕区间或画面，点击可回到原视频。摘要走固定 Map/Reduce，问答走 Eino 工具循环。

这是可运行的首版实现。访客无需注册或密码，打开页面即可使用。没有云端凭据时可以验证访客空间、Agent 管理、真实上传和持久任务；示例配置明确关闭 ASR/模型，不会编造字幕或答案。验证范围与剩余工作见 [实施状态](docs/implementation-status.md)。

原始依据：[开发文档](docs/video-agent-development-guide.md) · [技术实现方案](docs/video-agent-technical-design.md)。

## 技术与进程

- Go + go-zero HTTP，Eino ADK 单 Agent 工具循环；Vue 3 + TypeScript + Vite。
- MySQL 8.4 保存任务、幂等、租约、版本、检查点与事件；sqlx 显式事务。
- Redis 请求限流；Kafka + franz-go 的 Outbox/Inbox 分发，消费线程只接管任务。
- Elasticsearch：BM25 与向量查询都过滤 owner/video/content/build，应用层 RRF 合并。
- S3 存储视频与产物，本地 SeaweedFS；腾讯 ASR 使用云端私有 COS 暂存音频。FFmpeg/FFprobe/Tesseract 处理媒体。
- 四类进程：`api`、`worker -kind media`、`worker -kind agent`、`coordinator`。

当前无需 etcd 服务发现或领导选举，数据库租约支持多个 worker/coordinator；没有为了中间件数量额外加入 etcd。

## 克隆后在本地运行（推荐）

安装 Git、Python 3.9+ 和 Docker Engine/Desktop（含 Compose v2），启动 Docker。此方式将前端、Go 服务、FFmpeg/OCR 及中间件都放进容器，不需要先在宿主机安装 Go 或 Node。Windows 推荐在 WSL2 中执行；Podman 需额外安装 Compose provider。首次构建会下载镜像、Go/npm 依赖和媒体工具，建议给容器环境分配至少 8 GB 内存。

```bash
git clone https://github.com/MOLI0711/VideoAgentDemo.git
cd VideoAgentDemo
python3 deploy/local.py init
python3 deploy/local.py up
```

打开 **http://127.0.0.1:5173**，无需注册或密码。`up` 构建源码镜像，等待 MySQL/Redis/Kafka/ES 就绪，迁移数据库、初始化私有 S3 桶和 CORS，然后启动 API、media/agent Worker、coordinator 与前端 Nginx。只有网站与浏览器直传 S3 的端口绑定本机，数据库端口不对外发布。

初始化生成 `.runtime/container/config.yaml`（个人配置）、`env`（个人密钥）和 `s3.json`（本地存储身份）。这些文件全部忽略，不进入 Git；重复执行 `init` 保留已有设置和密钥。这套容器配置独立于下面的宿主机开发配置。

**默认不开通云服务，也不产生云模型请求。** 可以先体验访客空间、资料管理、真实上传与 Agent 配置；真实视频转写、总结和问答需要开发者填写自己的模型/ASR/COS API 配置，步骤见下一节。GitHub 使用者无需部署模型，也不会使用仓库所有者的私有 API Key。

```bash
python3 deploy/local.py check    # 检查真实本地依赖，不调用云模型
python3 deploy/local.py logs     # 最近的日志，已对配置中的凭据脱敏
python3 deploy/local.py status
python3 deploy/local.py down     # 停止本项目，保留数据卷与私有配置
```

改完代码后再次运行 `python3 deploy/local.py up`，会重新构建并刷新应用进程，加载个人配置，并更新前端代理的上游地址；已有中间件和数据卷保留。仅需启动已有镜像时使用 `up --no-build`。不要在保留数据卷时删除或重新生成密钥文件。

首次初始化需要换端口时，例如 `python3 deploy/local.py init --web-port 5180 --s3-port 18433`。请使用终端输出的地址，保持 `config.yaml` 的 `app.public_origin`、`storage.browser_endpoint` 与 `env` 中的对应端口一致。

如果使用 Podman，命令后加 `--engine podman`。若镜像拉取受网络限制，可在个人 `env` 增加 `VA_GO_IMAGE`、`VA_RUNTIME_IMAGE`、`VA_NODE_IMAGE`、`VA_NGINX_IMAGE`、`VA_MYSQL_IMAGE`、`VA_REDIS_IMAGE`、`VA_SEAWEED_IMAGE`、`VA_KAFKA_IMAGE`、`VA_ES_IMAGE` 指向你确认的同版本镜像仓库；不要更改提交的源码来解决个人网络问题。

## 本地完整 AI 功能：配置自己的云 API

编辑 **`.runtime/container/config.yaml` 和 `.runtime/container/env`**。这是每位开发者自己的配置，网站访客无需填写。

1. 在 `env` 填 `VA_MODEL_API_KEY`。在 YAML 将 `model.mode` 设为 `live`，填写供应商的兼容 API 根地址、`chat_model`、`vision_model` 和 `embedding_model`。当前 adapter 共用一个模型供应商地址和密钥；模型需支持应用使用的工具调用、JSON 输出、视觉或文本向量接口。
2. 在 `env` 填腾讯 ASR 的 `VA_ASR_SECRET_ID`、`VA_ASR_SECRET_KEY`，将 `asr.mode` 设为 `live`，选择已开通的地域和识别引擎。
3. 开通私有 COS 桶，在 `asr_stage` 填 `bucket`、`region` 和地域 endpoint，在 `env` 填 `VA_COS_SECRET_ID`、`VA_COS_SECRET_KEY`。云 ASR 读取此处暂存的音频；原始视频和本地处理产物仍可留在本地 SeaweedFS。
4. 将 `search.enabled` 设为 `true`，`search.dimensions` 填所选 embedding 模型实际支持的输出维度，设置固定 `embedding_profile`、独立 `index_name`。ES 已包含在容器环境中。更换模型或维度时换新 profile/index 并重新处理视频。
5. 再运行 `python3 deploy/local.py up`。需要主动验证 embedding 时运行 `python3 deploy/local.py provider-check`，该命令会请求云 API，可能计费；它不代表 chat/tools/vision/ASR 都已经通过。最后上传一段自己的有语音视频，完成真实端到端验证。

缺少账号时保持 `disabled` 即可，页面会显示能力未启用；有语音的视频处理会明确停在 `ASR_DISABLED`，不会给任意视频生成假字幕或假答案。浏览器中修改 Agent 的提示词、工具和版本，不会改变服务器的 API 密钥。

## 从源码开发（可选）

需要 Go ≥1.26、Node ≥22.12、Python 3、Docker Compose 或兼容 Podman 环境。媒体 worker 需要 FFmpeg、FFprobe、Tesseract 和 `chi_sim`/`eng` 语言包；`deploy/Dockerfile` 包含这些依赖。

```bash
cp configs/local.example.yaml configs/local.yaml
go run ./cmd/devtool -action init-env
docker compose --env-file .runtime/dev.env -f deploy/compose.yaml --profile full up -d
make migrate
make storage-init
npm --prefix web ci
```

`init-env` 只在文件不存在时生成随机凭据，保存为权限 0600 的 `.runtime/dev.env`、`.runtime/s3.json`，禁止提交 Git。这里的随机凭据仅供 MySQL/S3 基础设施使用；访客不需要用户名或密码。每个新访客自动拥有一个已发布的“视频助手”，无需 `make seed`。命令不回显基础设施密钥。DSN 含 `&`，使用 `deploy/with-env.py` 加载，不要直接 `source`。

应用可一条命令启动：

```bash
make dev
```

该命令先编译、迁移、初始化存储并检查依赖，再启动 API、两类 worker、coordinator 和前端；日志位于忽略的 `.runtime/dev-*.log`。Ctrl+C 只停止这次启动的应用进程，保留基础设施与数据。若需要分别调试，也可在多个终端运行：

```bash
make api
make media
make agent
make coordinator
make web
```

打开 `http://127.0.0.1:5173`，API 位于 `http://127.0.0.1:8080`。示例已启用 Redis 和 Kafka，需要同时运行 coordinator 与两类 worker。Elasticsearch 随 `full` 启动，但向量索引需接入真实 embedding 后才能启用。开发环境可显式改用 `db_dev`，production 配置禁止该模式。

示例关闭云端能力：worker 执行到 ASR/embedding 时会明确失败。完成真实配置后再重新处理视频。无声视频会跳过 ASR；当前分段依赖语音字幕，无字幕的视频会明确标记为不支持，不会生成虚构内容。

## 宿主机开发的真实服务配置

编辑忽略的 `configs/local.yaml`，密钥通过环境提供：

1. `model.mode: live`，填写供应商兼容 API root、chat/vision/embedding 模型标识；`VA_MODEL_API_KEY` 仅放环境。
2. `asr.mode: live`，提供腾讯 ASR 凭据；`asr_stage` 指定云端可访问的私有 COS bucket、region 和凭据。云端不能访问本机 S3 URL。
3. 启动 ES：`docker compose --env-file .runtime/dev.env -f deploy/compose.yaml --profile search up -d`。
4. `search.enabled: true`，填写经过验证的 embedding 维度、固定 profile 和独立 index 名。更换模型/维度时使用新 profile/index 并重新处理。
5. 先检查媒体与基础服务，再主动进行云模型检查：

```bash
make runtime-check
python3 deploy/with-env.py .runtime/dev.env go run ./cmd/devtool -action provider-check
```

`provider-check` 会调用真实 embedding，可能产生费用；不代表 ASR、工具调用和视觉均已通过。模型 adapter 已包含 tool calling、视觉、embedding、usage、超时与脱敏错误，没有预置付费账号或写死第三方模型名。

## Kafka / Redis

示例默认 `workflow.dispatch_mode: kafka`，`make coordinator` 负责 Outbox 发布、Kafka 消费与任务恢复。两个 topic 是 `video.media.dispatch`、`video.agent.dispatch`。本地允许自动创建 topic；线上需显式分区、副本、ACL。不要混用配置不同的 db_dev 与 Kafka 调度进程。

示例默认 `redis.enabled: true`，用 Redis 限流；不可用时写请求返回 503。Redis 不承担任务真相、最终幂等或会话互斥。

## 关键行为

- 无注册、无密码登录页。浏览器自动建立独立访客空间并保存 30 天 HttpOnly Cookie，刷新保持原身份；清除站点数据、换浏览器或会话到期后无法恢复原访客空间。已存在的有效 Cookie 仍可访问自己的旧数据。
- `POST /v1/auth/guest` 初始化或复用访客会话；密码登录端点已移除。CSRF、Origin 与 owner 隔离仍由服务端执行，访客不能读取其他人的视频或 Agent。
- 关键业务创建请求支持幂等键；浏览器保存分片清单和文件指纹，完成响应丢失后可重试同一 complete。
- 每次领取递增 `attempt_no`；续租、产物、事件、检查点与发布检查 lease/deadline/状态。取消即时撤销提交权，媒体进程在心跳发现后取消。
- Outbox 在事务外发送并按 publisher attempt 标记；Inbox + READY 持久化后才提交 Kafka offset。
- ASR 意图先落库，已知 provider job 只轮询；提交未知会记录 `SUBMISSION_UNKNOWN` 并安全终止，不盲目创建第二个付费任务。
- session/run 固定视频与 Agent 已发布版本；旧索引 attempt 不能覆盖最新 generation。
- QA 默认累计限制为 6 次模型、8 次工具、2 次视觉。摘要独立上限 96 次模型调用，每次最多请求 1800 输出 tokens，已完成 map/reduce 结果与累计次数可恢复。
- 文本模型调用另有持久预留/结算账本，未知用量不释放预算；输入采用字节数估计，不能描述为适配任意 tokenizer 的精确上界，视觉/embedding 不计入此文本账本。
- SSE 持久化 seq 支持 `after_seq`/`Last-Event-ID`；最终答案、消息、状态和终态事件同事务提交。
- 最终 claim 必须引用实际读过的证据 ID；结构校验不能保证语义绝对正确，需人工评测。

## 测试与评测

```bash
go test ./...
go test -race ./...
go vet ./...
npm --prefix web test
npm --prefix web run typecheck
npm --prefix web run build
npm --prefix web run format:check
python3 -m unittest discover -s deploy -p 'test_*.py'
python3 -m unittest discover -s eval
```

真实 MySQL 集成测试使用 `VA_MYSQL_TEST_ADMIN_DSN`，执行 `go test -tags=integration ./internal/repository/mysql ./internal/api ./internal/agent`。测试创建随机数据库，仅清理自身数据库，不要使用生产账号。Kafka 测试另设 `VA_KAFKA_TEST_BROKERS`。S3/媒体/检索测试变量见对应 `*_integration_test.go`。

[评测说明](eval/README.md)提供标注结构、指标脚本和固定检索 baseline。没有伪造真实视频准确率、成本或压测指标。

## 部署说明

Compose 是本地环境，端口仅绑定 loopback，ES/Kafka/Redis 没有生产认证。生产需要 TLS、独立账号、服务权限与持久化运维、SSE 代理不缓冲、私有桶与生命周期。模型密钥不会交给浏览器。

SeaweedFS 3.85 不支持 PutBucketCors，Compose 使用 `-s3.allowedOrigins`；预签名 S3 请求不携带 Cookie，ETag 可读行为已实测。源容器/编码可能不受浏览器支持，本版尚未统一转码为 H.264 播放副本。

开源许可为 [MIT](LICENSE)，开发和贡献流程见 [CONTRIBUTING.md](CONTRIBUTING.md)。

协作规则在 `.agents/AGENTS.md`，同步副本 `.claude/CLAUDE.md`。`internal/eventbus` 负责消息分发与接管；其余目录按 HTTP、仓储、工作流、媒体、检索、Agent 划分。原始设计保留在 `docs/`，以实施状态区分计划和已验证行为。
