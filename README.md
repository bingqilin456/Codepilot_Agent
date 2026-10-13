# CodePilot-Go

基于 Go、Gin 和 CloudWeGo Eino 的 Go 项目 Bug 定位与修复后端学习项目。

Phase 1 已完成用户运行验收：HTTP 基础服务和一个独立的模型工具调用命令行程序。当前尚未实现 HTTP 修复任务接口、任务管理、Docker 测试沙箱或真实仓库修复。

## 环境与固定版本

本次验证环境为 Windows/amd64、Go 1.27.1。

| 组件 | 版本或配置 |
|---|---|
| Gin | v1.12.0 |
| Eino | v0.9.21 |
| OpenAI 兼容模型适配器 | v0.1.13 |
| Sonic（间接依赖） | v1.15.4 |
| 模型 | deepseek-flash |
| Base URL | https://api.deepseek.com |

在项目根目录的 PowerShell 中下载依赖：

```powershell
go version
go mod download
```

## HTTP 服务

终端 A 启动：

```powershell
Remove-Item Env:CODEPILOT_HTTP_ADDR -ErrorAction SilentlyContinue
go run ./cmd/server
```

终端 B 查询：

```powershell
Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:8080/healthz' |
    Select-Object StatusCode, Content
```

预期为 HTTP 200、`{"status":"ok"}`。

监听地址通过 `CODEPILOT_HTTP_ADDR` 配置，未设置或为空时默认为 `127.0.0.1:8080`。需要修改地址时，在终端 A 按 Ctrl+C 停止服务，再执行：

```powershell
$env:CODEPILOT_HTTP_ADDR = '127.0.0.1:8081'
go run ./cmd/server
```

随后在终端 B 查询 `http://127.0.0.1:8081/healthz`。环境变量必须在启动服务的终端设置，修改后需要重启服务。

服务记录结构化启动配置与错误日志，配置请求头读取 5 秒、请求读取 10 秒、响应写入 10 秒、空闲连接 60 秒超时。这些是 HTTP 网络读写限制，不能代替 Agent 任务的超时与取消。

启动失败验证：停止旧服务后，在终端 A 执行：

```powershell
$env:CODEPILOT_HTTP_ADDR = '127.0.0.1:70000'
go run ./cmd/server
$LASTEXITCODE
Remove-Item Env:CODEPILOT_HTTP_ADDR -ErrorAction SilentlyContinue
```

预期出现包含 `invalid port` 的 ERROR 日志，退出码为 1。

## 最小模型工具调用

命令行入口为 `cmd/agent-smoke`。程序通过 DeepSeek 的 OpenAI 兼容接口完成一次工具调用闭环，关闭模型思考模式。单次运行最多调用模型 2 次、执行工具 1 次，共享 60 秒总时限，每次模型调用限制最多 512 个输出 Token。运行成功路径会请求真实模型 API。

首先编译：

```powershell
go build ./cmd/agent-smoke
```

在本地终端安全输入 API Key，然后运行；不要把实际密钥写入源码、文档或 Git：

```powershell
$deepSeekKey = Read-Host 'DeepSeek API Key' -AsSecureString
$env:DEEPSEEK_API_KEY = [System.Net.NetworkCredential]::new('', $deepSeekKey).Password
go run ./cmd/agent-smoke
```

预期输出包括工具实际执行记录及：

```text
PASS model_calls=2 tool_calls=1 answer=Go
```

`get_project_info` 只接受项目 `CodePilot-Go` 并返回预定义的语言信息；不访问目标仓库或执行目标代码。模型输出工具名与参数，由 Go 校验允许范围、调用预算及输入大小后执行。工具结果按调用 ID 回传，成功由程序检查实际执行次数和最终答案判定。

缺少配置的失败路径：

```powershell
Remove-Item Env:DEEPSEEK_API_KEY -ErrorAction SilentlyContinue
go run ./cmd/agent-smoke
$LASTEXITCODE
```

预期提示缺少 `DEEPSEEK_API_KEY`，退出码为 1；该路径在发起模型请求前失败。

## 验收记录与下一阶段

2026-10-13 用户回传的运行输出验证了：

- 默认地址、自定义地址均返回 HTTP 200 和预期 JSON。
- 非法端口以退出码 1 结束，并记录具体错误。
- 不完整请求头测试在约 4.9 秒后读到连接关闭。
- 模型调用程序编译成功，实际执行工具并输出 PASS。
- 缺少密钥时正确失败退出。

其他 HTTP 超时字段、60 秒模型调用总超时的触发行为，以及错误工具参数等失败路径尚未分别进行动态验收。验收数字来自用户提供的运行结果，助手核对了源码和依赖。

下一阶段实现授权示例仓库的独立工作区、路径校验、受控文件工具和最小 Docker 沙箱。首次执行目标仓库代码前，必须具备路径校验、沙箱、执行超时与迭代/重试上限。

详细记录见 [开发进度](docs/progress.md) 和 [架构草稿](docs/architecture.md)。
