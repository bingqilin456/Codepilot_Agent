# Phase 2-03：最小 Docker 测试入口与原始问题复现

日期：2026-10-19。执行方式：你亲手创建源码、运行和提交；导师只整理讲义，并在独立临时副本验证候选。下面的 03 源码尚未接入真实项目。

**目标：** 在固定示例的独立副本中，只执行一次受限容器测试，拿到真实退出码和输出，确认右端点 Bug 确实存在。

**架构：** 本批先用 `cmd/sandbox-smoke` 验证执行边界。入口固定源目录、镜像 ID 和测试命令，复用已验收的 Workspace；后续再抽入 `internal/sandbox` 并接入 Agent 工具。不会把 `capture` 这个宿主进程辅助函数暴露给模型。

**技术栈：** 沿用 Go 1.27.1 和现有模块，标准库加 Docker CLI，不增加 Go 依赖。镜像预先准备，运行阶段按本地 ID 锁定；首次运行所需镜像下载与真实容器执行均由你验收。

**设计依据：** [整体设计](architecture.md)第 2、4、5 节，以及导师提示词第四、第五节。教学执行方式已确定，继续按完整功能批次推进。

## 本批固定约束

| 项目 | 执行策略 |
|---|---|
| 授权范围 | 固定 `testdata/bugs/boundary`；Prepare 创建独立副本，仅挂载该副本 |
| 目标执行 | 一次 `go test`；零自动重试、零模型调用；不接收通用命令 |
| 文件系统 | 根文件系统只读，副本只读；只有受限 `/tmp` 等容器临时区可写 |
| 权限与网络 | UID/GID 65534，移除 capabilities，禁止提权，网络 none，不挂载 Docker socket |
| 资源 | 2 CPU、1 GiB 内存及同值 memory-swap、128 个进程／线程、512 MiB tmpfs |
| 时间 | 整体执行 150 秒；容器测试 120 秒；管理操作各 10 秒；清理独立最多 10 秒 |
| 测试二进制 | `-timeout=10s`；编译不包含在这个十秒内，但受外层 120 秒限制 |
| 输出 | stdout、stderr 分开，各保留原始前 4 KiB，并持续排空后续输入；JSON 转义后可更大 |
| 判定 | 容器正常结束、未 OOM、退出码 1、出现指定右端点断言、工作区 Diff 为空、清理成功 |

表中的时间是各 Context 的截止值，管道收尾另设一秒 WaitDelay。`go run` 对宿主入口的编译发生在程序启动前，不包含在程序内的 150 秒预算中。

权限、只读挂载与资源参数按 [Docker 容器运行文档](https://docs.docker.com/engine/containers/run/)及 [CLI 参数文档](https://docs.docker.com/reference/cli/docker/container/run/)核对。容器提供执行约束，本批不将其描述为对任意恶意代码的完整安全保证。

审查重点：挂载 CSV 参数注入、输出超限导致管道阻塞、非零退出与启动失败混淆、只结束 CLI 却遗留容器、把 OOM 或编译失败当成 Bug 复现。前四类中的宿主辅助行为由下面单元测试检查；真实容器资源、只读策略及清理效果仍需运行验收，真实容器超时／OOM 故障注入留给后续沙箱模块批次。

## 0. 先收尾 02 的提交

02 已有真实通过输出。当前 `AM` 表示新文件暂存后又修改；导师核对时，暂存区的两个新文件实际各只有一行 package 声明。重新暂存当前完整内容后再提交：

```powershell
# 精确暂存本批实现与教学记录，不使用 git add . 带入其他目录。
# cached 检查针对即将提交的版本，应看到完整实现和完整测试。
git add -- internal/workspace/workspace.go internal/workspace/inspect.go internal/workspace/inspect_test.go docs/progress.md docs/architecture.md docs/phase2-03-docker-smoke.md
git diff --cached --check
git diff --cached --stat
git commit -m "feat: add bounded workspace search and diff"
git log -1 --oneline
```

## 1. 创建宿主测试

- [ ] 创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\cmd\sandbox-smoke\main_test.go`，填入下面完整代码。新目录可以通过编辑器创建。

测试只运行它自己定义的受控辅助进程，不连接 Docker，不运行 Bug 示例；它们用真实子进程验证退出码、输出限额和取消，不用假的 Docker 响应代替集成验收。

```go
// 本批宿主测试只执行本测试文件中的受控辅助进程，不连接 Docker 或运行目标示例。
// 它们验证输出限额、进程退出与取消，以及挂载参数和镜像配置的拒绝边界。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLimitedBuffer 验证恰好达到上限、越过上限及继续排空的行为。
// 达到保留上限后仍须报告完整接收字节数，否则子进程可能因管道错误提前退出。
func TestLimitedBuffer(t *testing.T) {
	var b limitedBuffer
	input := []byte(strings.Repeat("x", 4096))
	if n, err := b.Write(input); err != nil || n != 4096 || len(b.data) != 4096 || b.truncated {
		t.Fatalf("恰好上限错误：n=%d len=%d truncated=%v err=%v", n, len(b.data), b.truncated, err)
	}
	if n, err := b.Write([]byte("yz")); err != nil || n != 2 || len(b.data) != 4096 || !b.truncated {
		t.Fatalf("超限排空错误：n=%d len=%d truncated=%v err=%v", n, len(b.data), b.truncated, err)
	}
}

// TestCaptureExitAndOutput 验证非零业务退出码与基础设施错误分开，两个输出流分别保留。
// 辅助进程输出远超限额，仍必须正常写完并以指定退出码结束。
func TestCaptureExitAndOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := capture(ctx, exe, "-test.run=^TestSmokeHelperProcess$", "--", "codepilot-smoke-helper", "exit")
	if err != nil || got.ExitCode != 7 || got.Stdout != strings.Repeat("x", 4096) || !got.StdoutTruncated || got.Stderr != "known-error" || got.StderrTruncated {
		t.Fatalf("进程结果错误：exit=%d stdout_len=%d stderr=%q err=%v", got.ExitCode, len(got.Stdout), got.Stderr, err)
	}
}

// TestCaptureCancellation 验证预取消与运行期间截止都会返回 Context 错误。
// 若错误地改用不带 Context 的命令，sleep 辅助进程将越过本测试的时间界限。
func TestCaptureCancellation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := capture(ctx, exe, "-test.run=^TestSmokeHelperProcess$"); !errors.Is(err, context.Canceled) {
		t.Fatalf("预取消错误：%v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = capture(ctx, exe, "-test.run=^TestSmokeHelperProcess$", "--", "codepilot-smoke-helper", "sleep")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("运行超时未及时结束：elapsed=%v err=%v", time.Since(started), err)
	}
}

// TestCaptureStartFailure 防止把可执行文件不存在等启动错误误报为测试失败。
// 未得到真实退出状态时使用 -1，不能返回看起来像成功的默认值 0。
func TestCaptureStartFailure(t *testing.T) {
	got, err := capture(context.Background(), filepath.Join(t.TempDir(), "missing-command"))
	if err == nil || got.ExitCode != -1 {
		t.Fatalf("启动错误分类错误：exit=%d err=%v", got.ExitCode, err)
	}
}

// TestMountSpec 验证仅接受普通绝对目录，并拒绝会改变 Docker CSV 参数结构的字符。
// 路径授权由调用处的固定 Prepare 决定；本测试针对参数编码和目录类型检查。
func TestMountSpec(t *testing.T) {
	dir := t.TempDir()
	got, err := mountSpec(dir)
	if err != nil || got != "type=bind,source="+dir+",target=/workspace,readonly" {
		t.Fatalf("只读挂载错误：%q err=%v", got, err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "../outside", dir + ",target=/escape", dir + "\"", dir + "\n", dir + "\r", dir + "\x00", file} {
		if _, err := mountSpec(name); err == nil {
			t.Fatalf("应拒绝挂载路径：%q", name)
		}
	}
}

// TestImageID 验证运行配置必须锁定本地镜像 ID，不能把标签、选项或任意文本当镜像。
// 镜像的来源仍须由操作者信任，格式检查不等于内容安全审计。
func TestImageID(t *testing.T) {
	if err := validateImageID("sha256:" + strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "golang:latest", "--privileged", "sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("g", 64)} {
		if err := validateImageID(value); err == nil {
			t.Fatalf("应拒绝镜像配置：%q", value)
		}
	}
}

// TestSmokeHelperProcess 只在测试明确传入内部标记时充当子进程，否则立即返回。
// 两种模式提供已知输出／退出码或等待状态，不读取仓库、不调用 Shell，也不执行目标代码。
func TestSmokeHelperProcess(t *testing.T) {
	args := os.Args
	if len(args) < 2 || args[len(args)-2] != "codepilot-smoke-helper" {
		return
	}
	switch args[len(args)-1] {
	case "exit":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 40000))
		fmt.Fprint(os.Stderr, "known-error")
		os.Exit(7)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	default:
		os.Exit(99)
	}
}
```

## 2. 创建接口骨架，观察失败

- [ ] 创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\cmd\sandbox-smoke\main.go`，暂时填入骨架。

```go
// 本骨架让宿主测试先编译，再因辅助能力尚未实现而失败。
// main 不执行命令，替换完整实现之前不会运行容器或目标代码。
package main

import (
    "context"
    "errors"
)

// commandResult 的字段供测试观察退出、输出及截断行为。
type commandResult struct {
    ExitCode int
    Stdout, Stderr string
    StdoutTruncated, StderrTruncated bool
}

// limitedBuffer 暂无捕获行为，完整实现必须在限额后继续排空输入。
type limitedBuffer struct {
    data []byte
    truncated bool
}

// Write 暂未实现，输出限额测试应失败。
func (b *limitedBuffer) Write(p []byte) (int, error) {
    return 0, errors.New("Write 尚未实现")
}

// capture 暂未实现，进程退出、取消及启动错误测试应失败。
func capture(context.Context, string, ...string) (commandResult, error) {
    return commandResult{}, errors.New("capture 尚未实现")
}

// mountSpec 暂未实现，只读挂载及参数检查测试应失败。
func mountSpec(string) (string, error) {
    return "", errors.New("mountSpec 尚未实现")
}

// validateImageID 暂未实现，固定镜像配置测试应失败。
func validateImageID(string) error {
    return errors.New("validateImageID 尚未实现")
}

// main 在骨架阶段不执行任何命令。
func main() {}
```

- [ ] 在项目根目录执行，观察 FAIL 原因包含“尚未实现”，无需单独回传：

```powershell
go test ./cmd/sandbox-smoke -count=1
```

## 3. 替换完整实现，再做宿主验收

- [ ] 用下面内容完整覆盖 `main.go`；骨架与实现不要同时保留。

```go
// 本命令只在固定示例的独立副本中，通过一次受限 Docker 测试复现原始问题。
// 不连接模型、不接受仓库路径或 Shell 命令；宿主测试不会自动运行 main。
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"codepilot-go/internal/workspace"
)

// 每条输出流保留 4 KiB；总预算含镜像检查和容器管理，另留有界清理时间。
// 测试二进制的十秒超时不含编译，容器执行另受 120 秒外层时限约束。
const (
	streamLimit = 4 * 1024
	totalLimit  = 150 * time.Second
	testLimit   = 120 * time.Second
	manageLimit = 10 * time.Second
)

// 镜像必须由可信操作者预先拉取并配置内容寻址 ID，运行阶段不自动拉取。
// ID 格式校验不证明来源可信，也不替代工具链版本核对。
var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// commandResult 分开记录退出码、输出前缀和截断状态。
// 非零退出可以是正常测试失败；启动、管道及 Context 错误由 error 单独报告。
type commandResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	ElapsedMS       int64  `json:"elapsed_ms"`
}

// limitedBuffer 保存前缀并持续排空超限输入，避免阻塞子进程。
// 两条流各用独立实例；命令结束后才读取结果。
type limitedBuffer struct {
	data      []byte
	truncated bool
}

// Write 超限后仍报告完整接收长度，遵守 io.Writer 契约。
// 限制保留字节数，不把截断伪装成子进程写入错误。
func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := streamLimit - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

// capture 使用参数列表直接启动进程，不经过 Shell，并限制输出保留大小。
// Context 终止的是 CLI 进程，远端 Docker 容器仍由调用者负责清理。
func capture(ctx context.Context, executable string, args ...string) (commandResult, error) {
	result := commandResult{ExitCode: -1}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// WaitDelay 限制退出后仍未结束的管道等待；每条流由各自复制协程写入。
	// Run 返回后再收集缓冲区，避免与输出写入并发访问。
	var stdout, stderr limitedBuffer
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	started := time.Now()
	err := cmd.Run()
	result.ElapsedMS = time.Since(started).Milliseconds()
	result.Stdout, result.Stderr = string(stdout.data), string(stderr.data)
	result.StdoutTruncated, result.StderrTruncated = stdout.truncated, stderr.truncated
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	// 取消优先于非零退出，不能把主动终止报告为普通测试失败。
	// 非零退出保留在 result；其他启动或 I/O 错误仍返回 error。
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, nil
	}
	return result, err
}

// mountSpec 拒绝相对路径、特殊目录及能改变 Docker CSV 结构的字符。
// 目录必须来自固定的 Prepare；本函数本身不是任意路径授权接口。
func mountSpec(dir string) (string, error) {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, ",\"\r\n\x00") {
		return "", errors.New("invalid mount directory")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("plain work directory required")
	}
	return "type=bind,source=" + dir + ",target=/workspace,readonly", nil
}

// validateImageID 要求本地镜像 ID，拒绝可变标签和非 ID 输入。
// 配置来自可信操作者，后续模型参数不能覆盖它。
func validateImageID(value string) error {
	if !imageIDPattern.MatchString(value) {
		return errors.New("set CODEPILOT_SANDBOX_IMAGE_ID to a trusted local sha256 image ID")
	}
	return nil
}

// management 执行固定的 Docker 管理操作，每次最多十秒且要求退出码为零。
// 目标测试使用独立的更长 Context，不经此管理入口启动。
func management(ctx context.Context, args ...string) (string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, manageLimit)
	defer cancel()
	result, err := capture(stepCtx, "docker", args...)
	if err != nil {
		return "", fmt.Errorf("docker management: %w", err)
	}
	if result.ExitCode != 0 || result.StdoutTruncated || result.StderrTruncated {
		return "", fmt.Errorf("docker management exit=%d stderr=%q", result.ExitCode, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// containerState 采用 inspect 的真实状态，不能仅靠 CLI 退出码判定测试结果。
// OOM、启动错误或仍在运行均不能冒充成功复现的目标测试失败。
type containerState struct {
	Status    string
	Running   bool
	ExitCode  int
	OOMKilled bool
	Error     string
}

// smokeReport 保存一次尝试的证据；JSON 编码会转义控制字符。
// 流限额针对原始字节，JSON 转义后的报告可能更大，尚不是模型工具结果契约。
type smokeReport struct {
	ImageID      string         `json:"image_id"`
	Container    string         `json:"container"`
	Workspace    string         `json:"workspace"`
	TestAttempts int            `json:"test_attempts"`
	Command      commandResult  `json:"command"`
	State        containerState `json:"state"`
	PatchEmpty   bool           `json:"patch_empty"`
}

// smoke 为固定示例创建副本，执行一次测试并核对预期失败，零自动重试。
// 清理使用独立十秒 Context，执行过期后仍会尝试停止并删除容器。
func smoke(ctx context.Context, imageID string) (report smokeReport, retErr error) {
	if err := validateImageID(imageID); err != nil {
		return report, err
	}
	report.ImageID = imageID
	imageOS, err := management(ctx, "image", "inspect", "--format", "{{.Os}}", imageID)
	if err != nil {
		return report, err
	}
	if imageOS != "linux" {
		return report, errors.New("Linux Go image required")
	}
	// 只从可信固定路径复制，不接受模型或命令行提供的宿主路径。
	// 成功保留副本供复核；Close 必须晚于最后的 Diff 检查。
	w, err := workspace.Prepare(ctx, filepath.Join("testdata", "bugs", "boundary"), "workspaces")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, w.Close()) }()
	if _, err := w.ListFiles(ctx); err != nil {
		return report, err
	}
	mount, err := mountSpec(w.Dir())
	if err != nil {
		return report, err
	}
	report.Workspace = w.Dir()
	report.Container = "codepilot-p2-03-" + strings.ToLower(rand.Text())
	// 创建前注册清理，覆盖部分成功后 CLI 失败的路径。
	// 只删除程序生成名称的本次容器；失败合并进返回值，不输出 PASS。
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), manageLimit)
		defer cancel()
		_, err := management(cleanupCtx, "container", "rm", "--force", report.Container)
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup %s: %w", report.Container, err))
		}
	}()
	// 只读挂载任务副本，不挂载真实仓库、用户目录或 Docker socket。
	// Go 在受限 /tmp 中编译并运行二进制，因此该 tmpfs 明确允许 exec。
	args := []string{
		"container", "create", "--name", report.Container, "--pull=never",
		"--label=codepilot.lesson=phase2-03", "--network=none", "--read-only",
		"--user=65534:65534", "--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		"--cpus=2", "--memory=1g", "--memory-swap=1g", "--pids-limit=128",
		"--ulimit=nofile=256:256", "--shm-size=16m", "--log-driver=none", "--no-healthcheck",
		"--mount", mount, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=512m,mode=1777",
		"--workdir=/workspace", "--env=HOME=/tmp", "--env=GOCACHE=/tmp/gocache",
		"--env=GOTMPDIR=/tmp", "--env=GOPATH=/tmp/gopath", "--env=GOPROXY=off",
		"--env=GOSUMDB=off", "--env=GOTOOLCHAIN=local", "--env=GOMAXPROCS=2",
		"--env=CGO_ENABLED=0", "--env=GOENV=off", "--env=GOFLAGS=",
		"--entrypoint=/usr/local/go/bin/go", imageID,
		"test", "-mod=readonly", "-count=1", "-p=1", "-parallel=1", "-timeout=10s", "-run=^TestContains$", ".",
	}
	if _, err := management(ctx, args...); err != nil {
		return report, err
	}
	// 固定一次 start，没有循环或自动重试；超时后上方 defer 强制清理。
	// CLI 退出码与容器状态分开记录，防止把 Docker 故障当目标测试反馈。
	testCtx, cancel := context.WithTimeout(ctx, testLimit)
	defer cancel()
	report.TestAttempts = 1
	report.Command, err = capture(testCtx, "docker", "container", "start", "--attach", report.Container)
	if err != nil {
		return report, fmt.Errorf("container test: %w", err)
	}
	stateJSON, err := management(ctx, "container", "inspect", "--format", "{{json .State}}", report.Container)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal([]byte(stateJSON), &report.State); err != nil {
		return report, err
	}
	if report.State.Running || report.State.Status != "exited" || report.State.OOMKilled || report.State.Error != "" {
		return report, errors.New("container did not finish as a normal test process")
	}
	// 具体断言和真实退出码共同证明复现；仅看到 FAIL 不足以通过本批。
	// 测试退出 1 是预期失败，宿主验证程序在正确复现并清理后退出 0。
	if report.State.ExitCode != 1 || !strings.Contains(report.Command.Stdout, "--- FAIL: TestContains") || !strings.Contains(report.Command.Stdout, "Contains(5, 1, 5) = false, want true") {
		return report, errors.New("expected right-endpoint failure was not reproduced; inspect JSON output")
	}
	patch, err := w.Diff(ctx)
	if err != nil {
		return report, err
	}
	report.PatchEmpty = patch == ""
	if !report.PatchEmpty {
		return report, errors.New("read-only test changed the workspace")
	}
	return report, ctx.Err()
}

// run 设置执行总预算，所有清理完成之后才输出 PASS。
// 镜像 ID 来自本地操作者配置；此入口不读取 .env，不连接模型。
func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), totalLimit)
	defer cancel()
	report, err := smoke(ctx, os.Getenv("CODEPILOT_SANDBOX_IMAGE_ID"))
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	if err != nil {
		return err
	}
	fmt.Println("PASS baseline_reproduced=true test_attempts=1 retries=0 cleanup=ok")
	return nil
}

// main 只把结果转换为宿主退出码，资源清理由内部函数先完成。
// 配置、执行、验收或清理错误都会非零退出。
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] 集中运行宿主检查：

```powershell
# 本段只执行项目自己的单元测试；main 不会被 go test 自动调用。
# 固定 Bug 示例仍位于 testdata 的独立模块中。
gofmt -w cmd/sandbox-smoke/main.go cmd/sandbox-smoke/main_test.go
go test ./cmd/sandbox-smoke -count=1 -v
go vet ./cmd/sandbox-smoke
go test ./... -count=1
git diff --check
```

预期新测试通过，02 和 01 原测试继续通过；已有两个 Windows 链接 SKIP 不记作补验成功。

## 4. 准备可信镜像，再首次执行目标代码

- [ ] 在项目根目录 PowerShell 准备与当前工具链一致的官方 Go 镜像。此步骤可能下载镜像，网络只用于准备；目标测试容器禁用网络。以下 tag 本轮未实际拉取，tag 可用性和镜像平台以你终端结果为准。

```powershell
# 先检查拉取是否成功，失败就停止，不使用 latest 或静默升级 go.mod。
# 后续保存本地镜像 ID，运行阶段配置 --pull=never。
docker pull golang:1.27.1
if ($LASTEXITCODE -ne 0) { throw '镜像准备失败，请保留 Docker 错误并停止本段' }
$lessonImageId = docker image inspect --format '{{.Id}}' golang:1.27.1
if ($LASTEXITCODE -ne 0) { throw '无法取得镜像 ID' }
$env:CODEPILOT_SANDBOX_IMAGE_ID = $lessonImageId.Trim()
docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}}' $env:CODEPILOT_SANDBOX_IMAGE_ID
docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $env:CODEPILOT_SANDBOX_IMAGE_ID | Select-String '^GOLANG_VERSION='
```

记录实际 ID、Linux 平台和 `GOLANG_VERSION=1.27.1`。本地镜像 ID 用于锁定此次运行内容，不等同于可跨机器拉取的 registry digest。

- [ ] 完成上述配置与宿主测试后，首次通过受限入口运行目标代码：

```powershell
# 此入口只创建并运行一次目标测试容器，正常完成后强制删除该容器。
# go run 在宿主编译的是验证入口，目标 Bug 示例在容器内编译和执行。
go run ./cmd/sandbox-smoke
$LASTEXITCODE
docker ps -a --filter 'label=codepilot.lesson=phase2-03' --format '{{.Names}}'
git diff -- testdata/bugs/boundary
```

期望 JSON 中：`state.ExitCode=1`、`state.OOMKilled=false`、`test_attempts=1`、`patch_empty=true`；`command.stdout` 含 `Contains(5, 1, 5) = false, want true`。随后输出：

```text
PASS baseline_reproduced=true test_attempts=1 retries=0 cleanup=ok
```

宿主退出码应为 0，容器列表与固定示例 Diff 均无输出。这里的 PASS 表示“准确复现并完成清理”，不是“Bug 已修复”；容器测试的退出码 1 是本批需要观察的证据。

## 5. 常见错误与机制

- 缺少镜像配置：在模型请求和目标执行之前就失败；本命令不会读取 `.env`。环境变量要在实际运行命令的同一个终端设置。
- `manifest unknown`、拉取超时或平台不匹配：先解决镜像准备，回传最早的 Docker 错误；不凭空更换工具链版本。
- `module requires Go ...`：镜像版本不满足示例 go.mod；`GOTOOLCHAIN=local` 与禁网策略会明确失败，不自动下载替代工具链。
- 挂载路径含逗号或引号：本批明确拒绝，不能删除校验；受控工作区父路径需使用不含这些字符的路径。
- 容器里 `/tmp` 权限或执行失败：保留 `mode=1777` 和 `exec`。Go 的临时目录包含构建产物与二进制，不能机械地把所有可写目录设为 noexec。[Go 命令环境变量](https://pkg.go.dev/cmd/go@go1.27.1#hdr-Environment_variables)、[Docker tmpfs 选项](https://docs.docker.com/engine/storage/tmpfs/)
- 编译超时或 OOM：是执行环境／预算失败，不是 Bug 复现。输出含阶段和容器状态，先定位资源使用，再调整方案并重新验收。
- 取消 CLI 后仍有容器：本代码独立执行 `docker container rm --force`。若 Docker 引擎不可达导致清理失败，程序返回错误和本次精确容器名；恢复连接后仅检查并清理该容器，不运行全局 prune。创建、启动和删除是分开的生命周期操作。[Docker create](https://docs.docker.com/reference/cli/docker/container/create/)、[Docker start](https://docs.docker.com/reference/cli/docker/container/start/)

`capture` 的非零退出码是数据，`error` 表示启动、取消等执行问题；Docker 容器的真实状态再由 inspect 确认。外层最终判定必须同时看原因和证据。

本批仍仅用于固定可信教学案例。真实容器的超时、取消、OOM 与只读写入拒绝尚未逐项注入验证，后续抽取沙箱模块时集中补齐；模型尚未获得执行目标测试的工具。

## 6. 提交与集中回传

- [ ] 上述正常复现及清理通过后提交；失败则保留最早错误，先不提交本批。

```powershell
git add -- cmd/sandbox-smoke docs/phase2-03-docker-smoke.md docs/progress.md docs/architecture.md
git diff --cached --check
git diff --cached --stat
git commit -m "feat: add bounded docker baseline smoke check"
git log -1 --oneline
```

一次回传：宿主详细测试结果、go vet 是否无错误、实际镜像 ID／平台／版本、验证入口 JSON 与 PASS／退出码、容器清理查询结果、最新 commit。无需重新验收 Docker 版本或调用模型 API。

复盘题：为什么 `exec.CommandContext` 返回超时后，还需要使用独立 Context 清理 Docker 容器？同时说明“目标测试退出 1”和“本批验证程序退出 0”各自代表什么。

导师验证范围：独立临时副本已观察骨架导致的预期失败；完整候选的宿主测试和 go vet 通过，沿用两个 Windows 链接 SKIP。没有拉取本批镜像，没有执行目标代码或连接模型；真实项目的 03 接入和 Docker 运行仍待你验收。
