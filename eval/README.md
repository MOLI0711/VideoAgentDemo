# 评测与复现

评测集需使用自录、获授权或可合法处理的视频。代码不包含未授权课程，也没有伪造准确率。建议先录制 5 段 2–10 分钟视频，分别覆盖讲解、代码、图示、无声画面、带背景噪声的讲解。合成测试只能证明工程行为，不能证明真实理解质量。

每段视频人工标注问题及原片毫秒区间。先建立 20 条固定回归，再扩至 100 条；同一视频的版本、embedding profile、Agent 版本必须一致。除非已配置凭据并主动执行，测试不会调用付费服务。

标注文件每行一个 JSON：

```json
{"id":"自定义样本ID","video_id":"真实视频UUID","content_version":"真实版本UUID","question":"人工编写的问题","category":"speech或visual或unanswerable","expected_intervals":[[1000,5000]]}
```

结果文件每行包含 `id`、`status`、`latency_ms`、`usage_known`、API 的 `result`。人工看过原片再添加 `human_grounded: true/false`，缺失时保持未知，不要默认为正确。真实 provider 未返回 usage 时，`usage_known` 必须为 false。

```bash
python3 eval/score.py .runtime/eval-gold.jsonl .runtime/eval-results.jsonl
python3 -m unittest discover -s eval
```

指标区分：引用区间 IoU≥0.3 的覆盖率、人工依据视频判断的支持率、P50/P95 完成时延、失败数、合成结果数和未知 usage 数。引用命中不等于语义正确；脚本不会自动给语义正确率。

`internal/agent.Engine.Baseline` 提供固定检索 + 一次模型生成的对照，`Engine.Run` 为 Eino 工具循环。两者共用 scope 过滤、Evidence 与最终回答校验。对照实验需分别记录调用次数和 provider usage，视觉问题再比较是否调用 `inspect_frames`。目前尚无真实数据集上的对照结果。
