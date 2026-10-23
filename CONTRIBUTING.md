# 本地开发与贡献

vedioagent 使用 MIT 许可证，见 [LICENSE](LICENSE)。可以克隆、运行、修改和分发项目；保留许可证和版权声明。第三方依赖与镜像仍适用各自许可证。

## 获取与运行

先 Fork [VideoAgentDemo](https://github.com/MOLI0711/VideoAgentDemo)，再克隆你自己的 Fork 并创建功能分支。希望仅运行项目时可以直接克隆上游的 `main` 分支；本地未提交的修改不会出现在远程仓库中。

推荐按照 [README](README.md#克隆后在本地运行推荐) 的 Docker 流程运行：

```bash
python3 deploy/local.py init
python3 deploy/local.py up
```

前端、API、媒体 Worker、Agent Worker、coordinator 和中间件都在本地容器中；媒体工具已包含在后端镜像。只使用云 API 进行模型推理和语音识别，不要求开发者自建模型服务。

每位开发者的 `.runtime/container/` 配置彼此独立。不要在 Issue、PR 或截图中附上私有 env、云密钥、完整带签名的存储 URL 或真实用户视频。没有云账号时，仍可运行基础功能、单元测试和明确的 fixture 集成测试；这些结果不等同于真实模型效果。

## 修改与反馈

- 前端界面：`web/src/`；容器入口与同源 API 代理在 `deploy/Dockerfile.web`、`deploy/nginx.local.conf`。
- HTTP 边界与协议：`internal/api/`、`api/openapi.json`；接口修改同时更新前端类型和 OpenAPI。
- 持久任务：`internal/workflow/`、`internal/eventbus/`、`internal/repository/mysql/`；数据库结构采用新的顺序迁移，保留已有数据。
- 媒体与云服务：`internal/media/`、`internal/pipeline/`、`internal/provider/`、`internal/retrieval/`、`internal/storage/`。
- Agent 与摘要：`internal/agent/`；提示词输入不等于系统指令，回答保留实际证据引用。

容器模式修改源码后，再运行 `python3 deploy/local.py up`，重新构建并更新应用；关闭时运行 `down`，数据卷保留。它使用编译后的前端，不提供宿主机热更新。需要 Go/前端断点或 Vite 热更新时，使用 README 的“从源码开发”流程，并先停止使用相同端口的容器。

镜像构建与启动失败时先运行 `python3 deploy/local.py logs`、`status`。如果是权限、端口、资源或镜像网络问题，请提供经过检查的报错和系统/架构版本。不要用清空数据卷、重新生成密钥或给任意文件放开权限来掩盖问题。

## 验证与提交

只改文档或文案时做相应检查；业务逻辑、权限与状态变更需要相关回归。Go、Node 和 Python 开发环境安装后可执行：

```bash
go test ./...
go vet ./...
npm --prefix web ci
npm --prefix web test
npm --prefix web run typecheck
npm --prefix web run format:check
npm --prefix web run build
python3 -m unittest discover -s deploy -p 'test_*.py'
python3 -m unittest discover -s eval
```

真实 MySQL、Kafka、S3、媒体和 ES 集成测试必须显式设置测试环境；测试仅创建和清理自己的随机资源。默认测试不调用付费模型。

提交前检查 diff 和未跟踪文件，确认不包含 `.runtime/`、`configs/local.yaml`、`node_modules/` 或构建产物。PR 描述说明问题、最终行为和实际验证；未运行的云服务或端到端测试明确注明。推送你自己的功能分支，再向上游提出 PR。
