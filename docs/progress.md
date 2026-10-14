# CodePilot-Go 开发进度

更新日期：2026-10-14

## 当前阶段与代码版本

- Phase 1：基础服务、最小 Tool Calling 及首次 Git 提交已完成；进入 Phase 2 第 1 批：独立工作区与路径校验。工作区代码已实现并完成该模块的助手验证；链接子测试待补验，完整集中验收尚未完成。
- 当前模块：`codepilot-go`。
- 用户回传实际 Go 工具链：`go version go1.27.1 windows/amd64`；与 `go.mod` 声明一致。
- 已安装依赖：Gin `v1.12.0`、Eino `v0.9.21`、OpenAI 兼容适配器 `v0.1.13`、Sonic `v1.15.4`、Sonic Loader `v0.5.2`；助手已核对当前 `go.mod`。
- Git 基线提交：`804bb14`（`feat: complete phase 1 service and tool calling`）；助手于 2026-10-14 再次核对 HEAD。工作区代码、注释与教学文档等改动尚未提交，暂存内容由用户管理。

## 已通过的验收

- Go Module 与 Gin 最小服务已实现，入口为 `cmd/server/main.go`。
- 默认地址 `127.0.0.1:8080/healthz` 和环境变量配置后的 `127.0.0.1:8081/healthz` 均返回 HTTP 200 和 `{"status":"ok"}`。
- 标准库 `slog` 启动配置日志包含实际监听地址及四个 HTTP 超时字段。
- 非法端口 `127.0.0.1:70000` 记录 ERROR 日志及 `invalid port`，退出码为 1。
- 不完整请求头测试：读取结果为 -1，耗时约 4.9 秒；与 5 秒请求头超时配置一致。
- 请求读取、响应写入和空闲连接超时字段已从代码核对，尚未分别做动态测试。
- `go build ./cmd/agent-smoke` 成功。
- DeepSeek `deepseek-flash` 模型实际发出工具调用，程序执行 `get_project_info` 并回传 `{"language":"Go"}`，最终输出 `PASS model_calls=2 tool_calls=1 answer=Go`。
- 清除 `DEEPSEEK_API_KEY` 后，程序在模型请求前报缺少环境变量，退出码为 1。
- 证据来源：用户于 2026-10-13 回传完整服务日志和 PowerShell 验收输出；助手已核对最新源码，未独立复现运行结果。
- 用户回传首次 Git 提交、`git diff --cached --check` 无错误，以及 Docker 客户端与引擎版本输出；引擎为 Docker Desktop 4.94.0 / Engine 29.8.2，容器类型为 `linux`。助手独立核对 Git 提交，未独立连接 Docker 引擎。

## 当前功能批次

- 环境变量配置、基础日志和 HTTP 服务超时批次已通过，源码由用户亲手修改。
- Eino 模型接入与最小受控 Tool Calling 已通过；Phase 1 技术目标完成。
- 用户已选择模型 `deepseek-flash`，OpenAI 兼容 Base URL 为 `https://api.deepseek.com`；API Key 仅由用户在本地配置，不提供给助手。
- 模型构造方法、工具接口和扩展请求字段已按上述固定版本官方源码核查。
- 最小调用验证：关闭模型思考模式，最多 2 次模型调用、1 次工具执行、共享 60 秒总时限；每次模型调用最多 512 个输出 Token。成功要求程序实际执行允许的工具并得到预期答案，不能只看模型声称调用了工具。
- 模型连通性、工具实际执行及缺少密钥的失败路径已由用户运行验证；助手未独立编译或调用付费模型。
- 首个工具只返回受控的预定义项目信息，不读取或执行目标仓库代码。
- 已补充可复现的 `README.md` 与 `.gitignore`；后者排除本地 IDE 配置、构建产物、密钥文件和后续任务工作区。
- 首次 Git 提交已完成，不再重复 Phase 1 验收或暂存步骤。
- Phase 2 第 1 批教学范围：固定授权的闭区间示例、每次创建独立目录、普通文件内容复制、严格读写白名单、`os.Root` 路径约束、64 KiB 单文件上限及取消检查；实现代码与集中验收步骤见 `docs/phase2-01-workspace.md`，由用户亲手创建源码和运行。
- 该批次暂不执行目标仓库代码；`testdata` 目录与独立模块用于避免主模块测试意外运行示例。最小 Docker 沙箱及执行/重试预算完成后才复现目标 Bug。
- 助手及审查子代理已静态复核本批讲义代码、测试与 Windows 句柄清理顺序；`os.Root`、`rand.Text` 和 Go 包发现规则已核对 Go 1.27.1 官方文档。此前助手终端未找到 Go；2026-10-14 已确认可用工具链为 `go1.27.1 windows/amd64`。
- 用户授权重写注释后，助手已为 `internal/workspace/workspace.go` 与 `workspace_test.go` 补齐包、函数和独立功能段的中文说明，覆盖职责、数据流、权限边界、清理顺序、取消检查及测试覆盖范围；已格式化，并用 Go scanner 比较修改前后的非注释词法单元，确认未改变代码逻辑或断言。
- 助手实际执行 `go test -mod=readonly ./internal/workspace -count=1 -v`，退出码为 0：副本隔离、路径拒绝、测试文件写保护、读写大小上限、预取消、创建失败清理及关闭后拒绝测试通过。`TestRejectSymlink/source` 与 `TestRejectSymlink/workspace` 因 Windows 缺少符号链接创建权限（错误 1314）跳过，不记为已验证。
- 助手实际执行 `go vet -mod=readonly ./internal/workspace`，退出码为 0；相关文件的 `git diff --check` 无空白错误。以上测试只操作临时文件中的字节，没有编译或执行目标 Bug 示例代码，也没有请求模型。

## 未解决问题与下一批次

- HTTP 服务超时仅控制网络读写，不能代替 Agent 任务的超时或取消。
- Windows PowerShell 验收命令使用 `Invoke-WebRequest -UseBasicParsing`，避免网页解析确认提示。
- 60 秒模型调用总超时的触发、错误工具参数等预算/拒绝路径尚未分别动态验证；代码已核对，不把这些情况记为测试通过。
- 尚未实现 HTTP Agent 接口、任务管理、Docker 测试沙箱或真实仓库修复。
- 当前待验收：主模块的完整集中检查与本批 Git 提交；两个链接子测试需在具有相应权限的环境中补验，可按讲义安排在后续 Linux 沙箱批次完成。读取时文件增长、检查后路径替换、运行中取消或超时及原子回滚尚无专门动态验收，不将本次测试结果扩展为这些能力的证明。
- 下一批在本批验收后接入检索/Diff，随后补齐最小 Docker 沙箱、执行超时及迭代/重试上限，再首次执行目标代码。
- 用户终端已确认 Docker 引擎可用；不要求重复验证 `docker version` 或容器类型。

## Phase 1 里程碑复盘

- 已覆盖岗位能力：Gin 服务与环境变量配置、结构化日志、HTTP deadline、Eino 模型适配、工具调用消息闭环、参数校验与调用预算。
- 面试题：模型返回一个工具调用后，真正执行工具的是谁？为什么最终成功仍要由外层程序判定？结合 `cmd/agent-smoke/main.go` 的执行计数与答案检查解释。
- README 或简历可记录的事实：已实现 Go/Gin 基础服务及 Eino/DeepSeek 工具调用验证程序；验证 2 次模型调用、1 次受控工具执行的闭环，并完成配置缺失和非法端口失败路径验收。尚不描述为真实仓库自动修复 Agent。
