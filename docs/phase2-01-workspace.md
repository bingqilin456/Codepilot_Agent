# Phase 2 第 1 批：独立工作区与路径校验

日期：2026-10-14。执行方式：你亲手创建源码、运行命令和提交 Git；导师提供本批完整代码并整理进度。本文中的 Go 文件尚未写入源码目录，运行验收尚未完成。

**目标：** 从固定授权的闭区间 Bug 示例创建独立副本；修改副本不会影响原始示例；文件接口拒绝越界路径、凭据文件和测试修改。

**设计：** 外层 Go 程序选择源目录和工作区父目录，模型以后只提交工作区内的文件名。首批采用严格白名单，只复制 `go.mod`、`range.go`、`range_test.go`，只允许写 `range.go`。文件实际访问通过 `os.Root`，复制使用新建文件而非硬链接。

**技术栈：** Go 1.27.1 标准库；沿用主模块 `codepilot-go`，不新增依赖，不改 Eino 或模型版本。

**需求依据：** [导师提示词](../CodePilot-Go_AI_Agentv2导师提示词.md) 第三至第五节，以及 [架构草稿](architecture.md) 的首个 Bug 与执行边界。

本批完成后，仍需下一批的检索/Diff、沙箱和执行预算，才能进入真实 Bug 的读取→修改→测试→反馈闭环。

## 本批约束与验收

- 输入源为我们编写并授权的固定示例，源目录与工作区父目录都是外层程序配置，不接受模型选择宿主路径。
- 文件名原样匹配白名单；不先 `Clean`，不把 `../range.go` 或 `./range.go` 修正成允许的文件名。
- 只复制三个普通文件，不带入 `.git`、`.env`、其他隐藏文件或链接。
- 单文件上限 64 KiB；读取实际限流，拒绝超限写入时原文件保持原样。
- 本批文件操作串行执行；取消在操作前后检查，不声称 `context` 能中断正在发生的普通文件系统调用。
- 创建失败先关闭工作区句柄，再通过父目录的 `os.Root.RemoveAll` 清理刚生成的子目录。
- 成功创建的工作区由调用者持有；`Close` 只释放句柄，保留结果。测试用 `t.TempDir` 清理自己的临时目录。
- 本批只运行工作区模块的可信测试，不执行示例仓库的测试。示例位于 `testdata` 且有独立 `go.mod`，避免主模块的 `./...` 自动发现它。[Go 命令规则](https://pkg.go.dev/cmd/go@go1.27.1#hdr-Package_lists_and_patterns)

审查重点：副本共享原文件、Windows 路径别名、根外符号链接、大小限制前先截断文件、创建失败遗留目录。下面的测试覆盖这些行为；Windows 无符号链接创建权限时会明确 SKIP，不把该项记为通过。

## 1. 创建目录和固定示例

所有命令在 `D:\Users\ZhuanZ1\Desktop\Eino_project` 的 PowerShell 执行：

```powershell
New-Item -ItemType Directory -Force -Path internal/workspace, testdata/bugs/boundary | Out-Null
```

创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\testdata\bugs\boundary\go.mod`：

```go
module codepilot-fixture-boundary

go 1.27.1
```

创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\testdata\bugs\boundary\range.go`：

```go
package boundary

// Contains 判断 n 是否属于闭区间 [min, max]，前提是 min <= max。
func Contains(n, min, max int) bool {
    return n >= min && n < max
}
```

这里故意保留右端点 Bug，首批不修复原始示例。

创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\testdata\bugs\boundary\range_test.go`：

```go
package boundary

import "testing"

func TestContains(t *testing.T) {
    cases := []struct {
        n    int
        want bool
    }{
        {0, false},
        {1, true},
        {3, true},
        {5, true},
        {6, false},
    }
    for _, tc := range cases {
        if got := Contains(tc.n, 1, 5); got != tc.want {
            t.Errorf("Contains(%d, 1, 5) = %v, want %v", tc.n, got, tc.want)
        }
    }
}
```

这个测试文件属于目标示例，本批仅作为字节复制。首次在 Docker 中执行时，预期 `n=5` 失败；届时再加入评测端保存的固定验收测试，不能仅以模型自述或这一个测试结果判定修复成功。

## 2. 先创建工作区验收测试

创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\workspace_test.go`：

```go
package workspace_test

import (
    "bytes"
    "context"
    "errors"
    "os"
    "path/filepath"
    "runtime"
    "syscall"
    "testing"

    "codepilot-go/internal/workspace"
)

// 把固定示例复制到测试专用临时源，后面的异常测试不修改真实示例。
func makeSource(t *testing.T) string {
    t.Helper()
    source := t.TempDir()
    fixture := filepath.Join("..", "..", "testdata", "bugs", "boundary")
    for _, name := range []string{"go.mod", "range.go", "range_test.go"} {
        data, err := os.ReadFile(filepath.Join(fixture, name))
        if err != nil {
            t.Fatal(err)
        }
        if err := os.WriteFile(filepath.Join(source, name), data, 0644); err != nil {
            t.Fatal(err)
        }
    }
    if err := os.WriteFile(filepath.Join(source, ".env"), []byte("FAKE_TEST_SECRET"), 0600); err != nil {
        t.Fatal(err)
    }
    return source
}

func prepare(t *testing.T, source, parent string) *workspace.Workspace {
    t.Helper()
    w, err := workspace.Prepare(context.Background(), source, parent)
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { _ = w.Close() })
    return w
}

func TestCopyIsIndependent(t *testing.T) {
    source, parent := makeSource(t), t.TempDir()
    first := prepare(t, source, parent)
    second := prepare(t, source, parent)
    if first.Dir() == second.Dir() || filepath.Dir(first.Dir()) != parent {
        t.Fatal("工作区必须是父目录下不同的独立目录")
    }
    entries, err := os.ReadDir(first.Dir())
    if err != nil || len(entries) != 3 {
        t.Fatalf("只应复制三个文件，entries=%v err=%v", entries, err)
    }
    for _, name := range []string{"go.mod", "range.go", "range_test.go"} {
        original, err := os.ReadFile(filepath.Join(source, name))
        if err != nil {
            t.Fatal(err)
        }
        copied, err := first.ReadFile(context.Background(), name)
        if err != nil || !bytes.Equal(copied, original) {
            t.Fatalf("复制 %s 不完整，err=%v", name, err)
        }
    }
    original, err := os.ReadFile(filepath.Join(source, "range.go"))
    if err != nil {
        t.Fatal(err)
    }
    changed := []byte("package boundary\n// workspace-only change\n")
    if err := first.WriteFile(context.Background(), "range.go", changed); err != nil {
        t.Fatal(err)
    }
    got, err := first.ReadFile(context.Background(), "range.go")
    if err != nil || !bytes.Equal(got, changed) {
        t.Fatalf("副本未正确写入，err=%v", err)
    }
    untouched, err := os.ReadFile(filepath.Join(source, "range.go"))
    if err != nil || !bytes.Equal(untouched, original) {
        t.Fatalf("源文件被改变，err=%v", err)
    }
    untouched, err = second.ReadFile(context.Background(), "range.go")
    if err != nil || !bytes.Equal(untouched, original) {
        t.Fatalf("另一工作区被改变，err=%v", err)
    }
}

func TestPathPolicy(t *testing.T) {
    w := prepare(t, makeSource(t), t.TempDir())
    denied := []string{
        "", "../range.go", "./range.go", "sub/../range.go",
        "/etc/passwd", "D:/secret.txt", "D:\\secret.txt",
        "sub\\range.go", ".env", "NUL", "range.go:secret", "RANGE.GO",
    }
    for _, name := range denied {
        if _, err := w.ReadFile(context.Background(), name); !errors.Is(err, workspace.ErrDenied) {
            t.Errorf("ReadFile(%q) 应拒绝，err=%v", name, err)
        }
        if err := w.WriteFile(context.Background(), name, []byte("x")); !errors.Is(err, workspace.ErrDenied) {
            t.Errorf("WriteFile(%q) 应拒绝，err=%v", name, err)
        }
    }
    for _, name := range []string{"go.mod", "range_test.go"} {
        before, err := w.ReadFile(context.Background(), name)
        if err != nil {
            t.Fatal(err)
        }
        if err := w.WriteFile(context.Background(), name, []byte("x")); !errors.Is(err, workspace.ErrDenied) {
            t.Fatalf("不允许修改 %s，err=%v", name, err)
        }
        after, err := w.ReadFile(context.Background(), name)
        if err != nil || !bytes.Equal(before, after) {
            t.Fatalf("被拒绝的写入改变了 %s，err=%v", name, err)
        }
    }
}

func TestFileSizeLimit(t *testing.T) {
    w := prepare(t, makeSource(t), t.TempDir())
    limit := bytes.Repeat([]byte("x"), 64*1024)
    if err := w.WriteFile(context.Background(), "range.go", limit); err != nil {
        t.Fatal(err)
    }
    got, err := w.ReadFile(context.Background(), "range.go")
    if err != nil || !bytes.Equal(got, limit) {
        t.Fatalf("64 KiB 应可读写，err=%v", err)
    }
    oversized := bytes.Repeat([]byte("x"), 64*1024+1)
    if err := w.WriteFile(context.Background(), "range.go", oversized); !errors.Is(err, workspace.ErrTooLarge) {
        t.Fatalf("超限写入应拒绝，err=%v", err)
    }
    got, err = w.ReadFile(context.Background(), "range.go")
    if err != nil || !bytes.Equal(got, limit) {
        t.Fatalf("超限写入不得截断旧文件，err=%v", err)
    }
    // 可信测试主动构造异常文件；这不是提供给模型的写入接口。
    if err := os.WriteFile(filepath.Join(w.Dir(), "range.go"), oversized, 0644); err != nil {
        t.Fatal(err)
    }
    if _, err := w.ReadFile(context.Background(), "range.go"); !errors.Is(err, workspace.ErrTooLarge) {
        t.Fatalf("超限读取应拒绝，err=%v", err)
    }
}

func TestCancelledContext(t *testing.T) {
    source, parent := makeSource(t), t.TempDir()
    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    if _, err := workspace.Prepare(ctx, source, parent); !errors.Is(err, context.Canceled) {
        t.Fatalf("取消后不应创建工作区，err=%v", err)
    }
    entries, err := os.ReadDir(parent)
    if err != nil || len(entries) != 0 {
        t.Fatalf("取消后不应留下目录，entries=%v err=%v", entries, err)
    }
    w := prepare(t, source, parent)
    before, err := w.ReadFile(context.Background(), "range.go")
    if err != nil {
        t.Fatal(err)
    }
    if _, err := w.ReadFile(ctx, "range.go"); !errors.Is(err, context.Canceled) {
        t.Fatalf("取消后不应读取，err=%v", err)
    }
    if err := w.WriteFile(ctx, "range.go", []byte("x")); !errors.Is(err, context.Canceled) {
        t.Fatalf("取消后不应写入，err=%v", err)
    }
    after, err := w.ReadFile(context.Background(), "range.go")
    if err != nil || !bytes.Equal(before, after) {
        t.Fatalf("取消写入改变了文件，err=%v", err)
    }
}

func TestPrepareFailureCleansDirectory(t *testing.T) {
    for _, failure := range []string{"missing", "oversized", "directory"} {
        t.Run(failure, func(t *testing.T) {
            source, parent := makeSource(t), t.TempDir()
            target := filepath.Join(source, "range_test.go")
            if err := os.Remove(target); err != nil {
                t.Fatal(err)
            }
            switch failure {
            case "oversized":
                if err := os.WriteFile(target, bytes.Repeat([]byte("x"), 64*1024+1), 0644); err != nil {
                    t.Fatal(err)
                }
            case "directory":
                if err := os.Mkdir(target, 0755); err != nil {
                    t.Fatal(err)
                }
            }
            w, err := workspace.Prepare(context.Background(), source, parent)
            if err == nil {
                _ = w.Close()
                t.Fatal("异常源文件应使创建失败")
            }
            if failure == "missing" && !errors.Is(err, os.ErrNotExist) {
                t.Fatalf("缺失文件错误分类错误：%v", err)
            }
            if failure == "oversized" && !errors.Is(err, workspace.ErrTooLarge) {
                t.Fatalf("大小错误分类错误：%v", err)
            }
            if failure == "directory" && !errors.Is(err, workspace.ErrNotRegular) {
                t.Fatalf("文件类型错误分类错误：%v", err)
            }
            entries, readErr := os.ReadDir(parent)
            if readErr != nil || len(entries) != 0 {
                t.Fatalf("创建失败后留下部分工作区，entries=%v err=%v", entries, readErr)
            }
        })
    }
}

func replaceWithSymlink(t *testing.T, filename, outside string) {
    t.Helper()
    if err := os.Remove(filename); err != nil {
        t.Fatal(err)
    }
    if err := os.Symlink(outside, filename); err != nil {
        if runtime.GOOS == "windows" &&
            (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(1314))) {
            t.Skipf("当前 Windows 无符号链接创建权限，本项尚未验证：%v", err)
        }
        t.Fatal(err)
    }
}

func TestRejectSymlink(t *testing.T) {
    for _, location := range []string{"source", "workspace"} {
        t.Run(location, func(t *testing.T) {
            source, parent := makeSource(t), t.TempDir()
            outside := filepath.Join(t.TempDir(), "outside.go")
            original := []byte("outside-file-must-stay-unchanged")
            if err := os.WriteFile(outside, original, 0644); err != nil {
                t.Fatal(err)
            }
            if location == "source" {
                replaceWithSymlink(t, filepath.Join(source, "range.go"), outside)
                if _, err := workspace.Prepare(context.Background(), source, parent); !errors.Is(err, workspace.ErrNotRegular) {
                    t.Fatalf("源链接应拒绝，err=%v", err)
                }
                return
            }
            w := prepare(t, source, parent)
            replaceWithSymlink(t, filepath.Join(w.Dir(), "range.go"), outside)
            if _, err := w.ReadFile(context.Background(), "range.go"); !errors.Is(err, workspace.ErrNotRegular) {
                t.Fatalf("工作区链接应拒绝读取，err=%v", err)
            }
            if err := w.WriteFile(context.Background(), "range.go", []byte("x")); !errors.Is(err, workspace.ErrNotRegular) {
                t.Fatalf("工作区链接应拒绝写入，err=%v", err)
            }
            after, err := os.ReadFile(outside)
            if err != nil || !bytes.Equal(after, original) {
                t.Fatalf("根外文件被修改，err=%v", err)
            }
        })
    }
}

func TestClosedWorkspace(t *testing.T) {
    source, parent := makeSource(t), t.TempDir()
    w, err := workspace.Prepare(context.Background(), source, parent)
    if err != nil {
        t.Fatal(err)
    }
    if err := w.Close(); err != nil {
        t.Fatal(err)
    }
    if _, err := w.ReadFile(context.Background(), "range.go"); err == nil {
        t.Fatal("关闭后不应读取")
    }
    if err := w.WriteFile(context.Background(), "range.go", []byte("x")); err == nil {
        t.Fatal("关闭后不应写入")
    }
}
```

## 3. 用最小接口骨架观察测试失败

先创建 `D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\workspace.go`，暂时放入下面的接口骨架。它用于让测试编译并因缺少功能而失败：

```go
package workspace

import (
    "context"
    "errors"
)

const MaxFileBytes = 64 * 1024

var (
    ErrDenied     = errors.New("workspace: file access denied")
    ErrTooLarge   = errors.New("workspace: file exceeds 64 KiB")
    ErrNotRegular = errors.New("workspace: regular file required")
)

type Workspace struct{}

func Prepare(ctx context.Context, sourceDir, parentDir string) (*Workspace, error) {
    return nil, errors.New("Prepare 尚未实现")
}
func (w *Workspace) Dir() string { return "" }
func (w *Workspace) Close() error { return nil }
func (w *Workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
    return nil, errors.New("ReadFile 尚未实现")
}
func (w *Workspace) WriteFile(ctx context.Context, name string, data []byte) error {
    return errors.New("WriteFile 尚未实现")
}
```

运行一次：

```powershell
go test ./internal/workspace -count=1
```

预期 FAIL，失败原因包含 `Prepare 尚未实现`。若是找不到示例文件或语法错误，先修正路径/粘贴错误再观察失败。无需为这一步单独回传或等待导师确认，直接完成下面的实现。

## 4. 替换骨架为完整实现

用下面代码完整替换 `D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\workspace.go`：

```go
package workspace

import (
    "context"
    "crypto/rand"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"
)

const MaxFileBytes = 64 * 1024

var (
    ErrDenied     = errors.New("workspace: file access denied")
    ErrTooLarge   = errors.New("workspace: file exceeds 64 KiB")
    ErrNotRegular = errors.New("workspace: regular file required")
)

type Workspace struct {
    dir  string
    root *os.Root
}

func (w *Workspace) Dir() string { return w.dir }

// Close 释放目录句柄，保留工作区内容供后续 Diff 和结果查询使用。
func (w *Workspace) Close() error { return w.root.Close() }

// Prepare 的两个目录参数只能来自外层程序的可信配置。
// 首版只支持我们编写并授权的 boundary 示例，不是通用仓库导入器。
func Prepare(ctx context.Context, sourceDir, parentDir string) (_ *Workspace, retErr error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    source, err := os.OpenRoot(sourceDir)
    if err != nil {
        return nil, fmt.Errorf("open source: %w", err)
    }
    defer source.Close()

    absoluteParent, err := filepath.Abs(parentDir)
    if err != nil {
        return nil, err
    }
    if err := os.MkdirAll(absoluteParent, 0700); err != nil {
        return nil, fmt.Errorf("create parent: %w", err)
    }
    parent, err := os.OpenRoot(absoluteParent)
    if err != nil {
        return nil, err
    }
    defer parent.Close()

    // 目录名由程序生成，Mkdir 碰到已存在目录直接失败，不复用它。
    taskName := "task-" + rand.Text()
    if err := parent.Mkdir(taskName, 0755); err != nil {
        return nil, err
    }
    var taskRoot *os.Root
    keep := false
    defer func() {
        if !keep {
            if taskRoot != nil {
                retErr = errors.Join(retErr, taskRoot.Close())
            }
            retErr = errors.Join(retErr, parent.RemoveAll(taskName))
        }
    }()
    taskRoot, err = parent.OpenRoot(taskName)
    if err != nil {
        return nil, err
    }

    for _, name := range []string{"go.mod", "range.go", "range_test.go"} {
        data, err := readRegular(ctx, source, name)
        if err != nil {
            return nil, fmt.Errorf("copy %s: %w", name, err)
        }
        if err := ctx.Err(); err != nil {
            return nil, err
        }
        // 每个副本都创建新文件，不能与源文件共享硬链接。
        file, err := taskRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
        if err != nil {
            return nil, err
        }
        n, writeErr := file.Write(data)
        if writeErr == nil && n != len(data) {
            writeErr = io.ErrShortWrite
        }
        if err := errors.Join(writeErr, file.Close()); err != nil {
            return nil, err
        }
    }
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    keep = true
    return &Workspace{dir: filepath.Join(absoluteParent, taskName), root: taskRoot}, nil
}

// 业务权限与目录约束是两层检查：先限制可访问的文件，再实际访问。
// 原样匹配会拒绝路径穿越、绝对路径、Windows ADS、大小写及分隔符别名。
func checkName(name string, write bool) error {
    if name == "range.go" || (!write && (name == "go.mod" || name == "range_test.go")) {
        return nil
    }
    return ErrDenied
}

func (w *Workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
    if err := checkName(name, false); err != nil {
        return nil, err
    }
    return readRegular(ctx, w.root, name)
}

func readRegular(ctx context.Context, root *os.Root, name string) ([]byte, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    info, err := root.Lstat(name)
    if err != nil {
        return nil, err
    }
    if !info.Mode().IsRegular() {
        return nil, ErrNotRegular
    }
    file, err := root.Open(name)
    if err != nil {
        return nil, err
    }
    defer file.Close()
    info, err = file.Stat()
    if err != nil {
        return nil, err
    }
    if !info.Mode().IsRegular() {
        return nil, ErrNotRegular
    }
    if info.Size() > MaxFileBytes {
        return nil, ErrTooLarge
    }
    // 即使打开后文件变大，实际读取也最多 MaxFileBytes+1 字节。
    data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
    if err != nil {
        return nil, err
    }
    if len(data) > MaxFileBytes {
        return nil, ErrTooLarge
    }
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    return data, nil
}

func (w *Workspace) WriteFile(ctx context.Context, name string, data []byte) (retErr error) {
    if err := ctx.Err(); err != nil {
        return err
    }
    if err := checkName(name, true); err != nil {
        return err
    }
    if len(data) > MaxFileBytes {
        return ErrTooLarge
    }
    info, err := w.root.Lstat(name)
    if err != nil {
        return err
    }
    if !info.Mode().IsRegular() {
        return ErrNotRegular
    }
    // 不使用 O_TRUNC：先核对打开的文件，再检查取消，最后才截断。
    file, err := w.root.OpenFile(name, os.O_WRONLY, 0)
    if err != nil {
        return err
    }
    defer func() { retErr = errors.Join(retErr, file.Close()) }()
    info, err = file.Stat()
    if err != nil {
        return err
    }
    if !info.Mode().IsRegular() {
        return ErrNotRegular
    }
    if err := ctx.Err(); err != nil {
        return err
    }
    if err := file.Truncate(0); err != nil {
        return err
    }
    n, err := file.Write(data)
    if err != nil {
        return err
    }
    if n != len(data) {
        return io.ErrShortWrite
    }
    return ctx.Err()
}
```

## 5. 集中验收

完成五个文件后执行：

```powershell
gofmt -w internal/workspace/workspace.go internal/workspace/workspace_test.go testdata/bugs/boundary/range.go testdata/bugs/boundary/range_test.go
go test ./internal/workspace -count=1 -v
go vet ./internal/workspace
go test ./... -count=1
git diff --check
git status --short
```

预期：工作区测试显示 PASS，包路径为 `codepilot-go/internal/workspace`；`go vet` 无错误。主模块测试可以包含 server/agent-smoke 的编译与工作区测试，不能列出或执行 `codepilot-fixture-boundary`。它不会调用 `main`，不会请求模型 API。`git diff --check` 不应报告空白错误；新建文件尚未暂存时，其内容由 gofmt/编译检查，Git 暂存后再做完整检查。

若 `TestRejectSymlink` 出现 SKIP，回传时保留原因；其余测试通过只代表其余项已验证。稍后的 Linux 沙箱批次再补齐链接测试，不要求为本批开启 Windows 管理员权限。

常见定位入口：

- `open ../../testdata/...: ...`：确认三个示例文件放在项目根目录的 `testdata/bugs/boundary`，不是 `internal/workspace/testdata`。
- `undefined: os.Root`、`undefined: rand.Text`：核对当前终端的 `go version` 与 `go.mod`，本批固定 Go 1.27.1；不要改成路径前缀判断来绕过。
- `Access is denied` 或临时目录删不掉：保留最早的报错，优先检查 `Close` 与失败清理顺序，不能靠强制删除原仓库解决。
- 示例右端点失败：本批不该运行目标示例；确认测试命令和当前目录，首次复现属于沙箱批次。

验收通过后，再由你提交本批：

```powershell
git add -- internal/workspace testdata/bugs/boundary docs/progress.md docs/phase2-01-workspace.md
git diff --cached --check
git diff --cached --stat
git commit -m "feat: add isolated fixture workspace and file policy"
git log -1 --oneline
```

集中回传 `go test ./internal/workspace -count=1 -v`、`go test ./... -count=1` 的输出，以及 `go vet` 是否无错误；若已提交，附最后一条 commit。不要回传 API Key，不需要再次调用模型或检查 Docker 版本。出现失败时保留失败项和完整错误，不先提交。

## 代码中的关键机制

1. **源目录、副本、模型文件名分别由谁控制。** 原始示例和父目录来自外层配置；`taskName` 由 Go 生成；模型以后只得到 `ReadFile`/`WriteFile` 这样的受控能力。`Prepare` 和 `Dir` 不应注册成让模型选择宿主路径的工具。
2. **白名单与 `os.Root` 分别负责什么。** 白名单决定业务上允许哪个文件、允许读还是写；`os.Root` 限制实际文件访问不逃出目录。`Lstat` 拒绝链接；打开后再次检查普通文件；实际读写仍通过根句柄。Go 官方说明也指出，`os.Root` 本身允许根内链接，不能解释成“禁止所有链接”。[Go 1.27.1 文档](https://pkg.go.dev/os@go1.27.1#Root)
3. **为何不用 Join 后做字符串前缀判断。** 同名前缀目录、Windows 路径语义、链接和检查后路径变化都可能让这种方法失效。我们既不规范化非法名称，也不把“先检查再用普通 os.Open”作为目录边界。[Go 官方解释](https://go.dev/blog/osroot)
4. **为何先判断大小再打开写入。** `O_TRUNC` 会在打开时截断文件；如果随后才发现参数过大，拒绝也来不及。当前实现先检查大小、文件类型和取消，再截断已打开文件。
5. **为何多读一个字节。** `LimitReader(max+1)` 区分“刚好 64 KiB”与“仍有更多内容”，限制实际分配；仅 Stat 大小不足以约束打开后增长的文件。
6. **为何创建随机目录时仍用 Mkdir。** `rand.Text()` 由标准库生成名字，`Mkdir` 确保存在则报错，不覆盖或复用别的任务目录。[Go 1.27.1 rand.Text](https://pkg.go.dev/crypto/rand@go1.27.1#Text)

本批适用范围是单机、固定授权示例、串行文件操作，假定宿主同权限进程不会并发篡改源或工作区。`os.Root` 也不隔离挂载点或设备文件。当前写入不是断电/崩溃时的原子事务；实际 I/O 失败可能留下不完整副本。它不能代替 Docker 沙箱、不可修改的评测测试、执行超时或预算。后续容器执行阶段必须把目标源码与验收测试的挂载和可写目录单独设计。

批次复盘题：`WriteFile("range_test.go", ...)` 为什么由外层程序拒绝，而不能只在 System Prompt 中写“不要修改测试”？结合文件白名单和模型参数不可信解释。通过本批后即可进入检索与 Diff 批次。
