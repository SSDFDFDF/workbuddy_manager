<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy Manager" width="120">
</p>

<h1 align="center">WorkBuddy Manager</h1>

<p align="center">
  <b>把腾讯 CodeBuddy / WorkBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  多账号池 · 熔断冷却 · 会话粘性 · 定时任务 · 流式 / 非流式
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
</p>

---

> **上游关系**
>
> - **直接上游**：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（原版增强分支；本仓库是其 fork，已同步基线 `d66384d` / `v1.13.0-panel`）
> - **原始上游**：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（原版仓库已删除，增强分支已同步至删库前最后一次更新 `ea8b1e5`）
> - 本 fork 相对直接上游的差异（转发契约、提示词体系、指纹改写层等）记录在 **[FORK_CHANGES.md](FORK_CHANGES.md)**。

## 项目简介

WorkBuddy Manager 是一个自托管的 **OpenAI / Anthropic 协议桥接网关**，将腾讯 CodeBuddy / WorkBuddy（`www.workbuddy.cn` / `www.workbuddy.ai`）账号包装为统一的 API：原生 `/v1/chat/completions`，以及无状态文本 / 工具子集的 `/v1/responses`、`/v1/messages`，并自带一个内嵌的 Web 管理面板。

- **账号**：面板内 OAuth 设备授权一键登录，凭证落盘并热加载进池（免重启）；多账号共享、单号故障自动换号。
- **调度**：积分加权选号、429/402 分级冷却与熔断、会话粘性绑定、在途租约限流。
- **协议**：出站强制流式、SSE 帧按规范重建，非流式由本地聚合；图片输入（Chat / Responses / Messages）统一归一为上游 data URL 形态。
- **运维**：签到与保活等定时任务、每请求表格日志、池状态原子落盘（可选 Upstash Redis 镜像）、配置在线热更新。
- **面板**：账号运维、用量与积分分析、模型档位查询、运行日志、任务中心（待办扫描 / 执行队列 / 活动目录按需拉取）、成长任务一键完成（哪些任务参与自动化由 `growth.autotasks` 配置，面板可改、保存即热生效）。

> ⚠️ 本项目仅限自用账号（签到 / 保活 / 个人工具接入），**不支持也禁止**批量小号分发额度、二次打包或收费售卖；使用前请阅读 [FORK_CHANGES.md](FORK_CHANGES.md) 与下方免责声明。

## 快速开始

```bash
# Docker Compose：准备 config.json 后启动，面板 http://127.0.0.1:7863/panel/
cp config.example.json config.json && docker compose up -d --build

# 或单文件二进制（Windows / macOS / Linux），首次启动自动生成含随机 api_key 的 config.json
./workbuddy_manager
```

从源码构建需 Go ≥ 1.22（`make build`）；配置字段见 [config.example.json](config.example.json)。协议兼容边界与客户端特征分析见 [docs/](docs/)。

配置只有一个来源：`config.json`（首次启动会按内置推荐值自动生成）。读取时只认当前 schema 的键，键名不认识就忽略并在日志/面板提示；保存（面板改配置）会按当前结构**整份覆盖**写回，因此**手写时拼错的键会在下次保存后消失**。程序不做版本迁移、不兼容旧键旧取值：手改配置前建议先备份，旧格式键名对照见 [FORK_CHANGES.md](FORK_CHANGES.md) §4。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发（源码或二进制形式）时，请保留原仓库的 MIT 版权声明与许可声明（如在 NOTICE 或 README 中注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`）
- 本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利；使用者仍需自行遵守上游服务条款
