# Phase 2 第 2 批：文件列表、关键词检索与 Diff

日期：2026-10-14。执行方式：你亲手修改项目源码、运行验收并提交 Git；导师提供完整带模块注释的实现和测试。本讲义中的 02 功能尚未写入真实项目源码。

**目标：** 在独立工作区内列出实际允许文件、按关键词定位相关行、生成可由 Git 消费的 Patch；同时检查文件集合与修改范围。

**架构：** 复用 Workspace 的根句柄、白名单、64 KiB 文件限制和 Context。Prepare 保存初始字节到私有内存，Diff 独立对照该基准，避免重新读取变化后的源目录。所有检索与 Diff 都不执行目标代码。

**技术栈：** Go 1.27.1 标准库，不新增依赖。Git 已是项目依赖，本批仅由可信测试在临时目录检查和应用 Patch；不开放 Shell 给模型。导师验证环境的 Git 为 2.52.0.windows.1。

**需求依据：** 导师提示词第二至第五节、架构草稿，以及已通过的 01 批次工作区接口。

## 当前验收与复盘

01 批次已完成集中验收，用户回传的工作区测试、go vet、主模块 go test ./... 和 Git 暂存检查均通过。两个链接子测试因 Windows 权限不足 SKIP，仍需在 Linux 环境补验。源码在 f3fdf15，讲义在 ec29c2d；不重写现有提交历史。

你的复盘抓住了权限由外层程序执行这一点，措辞需收紧：没有本项目统计支撑“模型大概率会改测试”；ErrDenied 是工具 API 的权限拒绝，并非宿主文件系统的物理隔离。其他直接文件访问或可写挂载能够绕开这个入口，所以仍需独立修改范围检查、不可修改的验收测试与 Docker 边界。

## 本批约束与接口

| 项目 | 固定规则 |
|---|---|
| 文件集合 | 恰好 go.mod、range.go、range_test.go 三个普通文件，不含额外文件或子目录 |
| 读写权限 | 三文件可读，只允许修改 range.go，沿用 01 的原样白名单 |
| 列表 | 真实目录枚举最多四项，检查集合与类型，成功结果按名称排序 |
| 关键字 | 大小写敏感的字面搜索，1～128 个 UTF-8 字节，不含 CR/LF/NUL；空格可查 |
| 匹配 | 每行一条，行号从 1 开始，最多 20 条 |
| 预览 | 最多 256 字节，保持完整 UTF-8 字符；line_truncated 指示行预览缩短 |
| 检索 JSON | 实际 json.Marshal 后最多 8 KiB；truncated 指示有整条匹配被省略 |
| Diff | 与 Prepare 时的快照比较，只为 range.go 输出整文件替换的一个 unified hunk |
| Patch 上限 | 完整 Patch 最多 8 KiB，超限返回 ErrOutputLimit，不输出半份 Patch |
| 生命周期 | 三个新入口均遵守预取消、关闭后拒绝；Diff 要在 Close 前生成 |

三个公开接口：

- ListFiles(ctx) ([]string, error)
- Search(ctx, keyword) (SearchResult, error)
- Diff(ctx) (string, error)

检索遇到条数或 JSON 限额会提前停止，返回的是有明确截断标记的前缀结果，不保证已经扫描后续所有文件。Diff 仍独立检查完整的三文件集合与保护文件。

审查重点及对应测试：同数量错误名称（TestListFiles/replacement）、JSON 转义放大（TestSearchOutputLimits）、基准漂移（TestDiffBaseline）、绕过写接口修改保护文件（TestDiffRejectsOutOfScope）、空文件/末尾换行/CRLF（TestDiffPatchRoundTrip）。

## 1. 新建测试文件

创建 D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\inspect_test.go，完整复制以下代码。它复用已有 workspace_test.go 的 makeSource 与 prepare，不重复定义这些辅助函数。

```go

// 新测试从公开接口验收列表、字面检索及 Patch，继续复用 01 批次的临时样本辅助函数。
// 所有文件修改与 Git 应用都在测试临时目录内，不编译或执行目标示例代码。
package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codepilot-go/internal/workspace"
)

// TestListFiles 检查稳定列表以及额外项、缺失项和同名目录的拒绝行为。
// 名字清单与实际目录必须一致，不能只返回硬编码名称而忽略磁盘状态。
func TestListFiles(t *testing.T) {
	w := prepare(t, makeSource(t), t.TempDir())
	got, err := w.ListFiles(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"go.mod", "range.go", "range_test.go"}) {
		t.Fatalf("列表错误：%v err=%v", got, err)
	}

	// 每个子测试使用独立副本，避免异常目录布局影响其他断言。
	for _, kind := range []string{"extra", "missing", "replacement", "directory"} {
		t.Run(kind, func(t *testing.T) {
			w := prepare(t, makeSource(t), t.TempDir())
			name := filepath.Join(w.Dir(), "range.go")
			want := workspace.ErrUnexpectedFiles
			if kind == "extra" {
				if err := os.WriteFile(filepath.Join(w.Dir(), ".env"), []byte("FAKE"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				// 用另一个文件替换被删除的允许项，数量仍然为三，必须检查名称集合才会拒绝。
				if kind == "replacement" {
					if err := os.WriteFile(filepath.Join(w.Dir(), "unknown.txt"), []byte("FAKE"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "directory" {
					want = workspace.ErrNotRegular
					if err := os.Mkdir(name, 0755); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := w.ListFiles(context.Background()); !errors.Is(err, want) {
				t.Fatalf("应拒绝异常布局，want=%v err=%v", want, err)
			}
		})
	}
}

// TestSearchBasic 验证字面匹配、大小写、行号和每行只返回一次的规则。
// 模板写入发生在临时副本，只用于检索，不要求它是可执行源码。
func TestSearchBasic(t *testing.T) {
	w := prepare(t, makeSource(t), t.TempDir())
	content := []byte("first\r\nneedle.[x] needle.[x]\r\n最后needle.[x]\r\n")
	if err := w.WriteFile(context.Background(), "range.go", content); err != nil {
		t.Fatal(err)
	}
	got, err := w.Search(context.Background(), "needle.[x]")
	want := []workspace.SearchMatch{
		{File: "range.go", Line: 2, Text: "needle.[x] needle.[x]"},
		{File: "range.go", Line: 3, Text: "最后needle.[x]"},
	}
	if err != nil || got.Truncated || !reflect.DeepEqual(got.Matches, want) {
		t.Fatalf("检索结果错误：%+v err=%v", got, err)
	}
	// 大小写不匹配时返回空数组，不能用正则语义解释关键字中的点与方括号。
	got, err = w.Search(context.Background(), "Needle.[x]")
	if err != nil || got.Truncated || got.Matches == nil || len(got.Matches) != 0 {
		t.Fatalf("无命中结果错误：%+v err=%v", got, err)
	}
}

// TestSearchQueryLimits 验证关键字字节上限和不支持的输入格式。
// 字面空格是允许的查询；空串、跨行或非法编码的查询应明确拒绝。
func TestSearchQueryLimits(t *testing.T) {
	w := prepare(t, makeSource(t), t.TempDir())
	for _, keyword := range []string{strings.Repeat("x", 128), " "} {
		if _, err := w.Search(context.Background(), keyword); err != nil {
			t.Fatalf("合法查询被拒绝：%v", err)
		}
	}
	for _, keyword := range []string{"", strings.Repeat("x", 129), "x\n", "x\r", "x\x00", string([]byte{0xff})} {
		if _, err := w.Search(context.Background(), keyword); !errors.Is(err, workspace.ErrInvalidQuery) {
			t.Fatalf("非法查询应拒绝，err=%v", err)
		}
	}
}

// TestSearchOutputLimits 检查命中数量、UTF-8 行预览、实际 JSON 字节数和长行边界。
// 结果截断表示省略整条命中，行预览截断由 LineTruncated 单独表示。
func TestSearchOutputLimits(t *testing.T) {
	w := prepare(t, makeSource(t), t.TempDir())
	// 恰好 20 条不能标记有遗漏，出现第 21 条时才触发数量截断。
	for _, count := range []int{20, 21} {
		if err := w.WriteFile(context.Background(), "range.go", []byte(strings.Repeat("needle\n", count))); err != nil {
			t.Fatal(err)
		}
		got, err := w.Search(context.Background(), "needle")
		if err != nil || len(got.Matches) != 20 || got.Truncated != (count == 21) {
			t.Fatalf("数量限制错误：%+v err=%v", got, err)
		}
	}
	// 中文字符可能跨过第 256 字节；预览必须缩短到完整字符边界。
	if err := w.WriteFile(context.Background(), "range.go", []byte(strings.Repeat("界", 100)+"needle\n")); err != nil {
		t.Fatal(err)
	}
	got, err := w.Search(context.Background(), "needle")
	if err != nil || len(got.Matches) != 1 {
		t.Fatalf("中文检索失败，err=%v", err)
	}
	match := got.Matches[0]
	if !match.LineTruncated || len(match.Text) > 256 || !utf8.ValidString(match.Text) || got.Truncated {
		t.Fatalf("UTF-8 预览或标记错误：%+v", got)
	}
	// JSON 默认转义会放大 < 字符，必须按真实编码结果限制输出，而非只数原始文本。
	if err := w.WriteFile(context.Background(), "range.go", []byte(strings.Repeat(strings.Repeat("<", 200)+"needle\n", 20))); err != nil {
		t.Fatal(err)
	}
	got, err = w.Search(context.Background(), "needle")
	encoded, encodeErr := json.Marshal(got)
	if err != nil || encodeErr != nil || len(encoded) > 8*1024 || !got.Truncated || len(got.Matches) == 0 {
		t.Fatalf("JSON 输出限制错误：bytes=%d result=%+v err=%v", len(encoded), got, err)
	}
	// 文件恰好 64 KiB 且整份只有一行时，Scanner 的缓冲区也必须允许这条合法输入。
	if err := w.WriteFile(context.Background(), "range.go", []byte("needle"+strings.Repeat("x", 64*1024-6))); err != nil {
		t.Fatal(err)
	}
	got, err = w.Search(context.Background(), "needle")
	if err != nil || len(got.Matches) != 1 || !got.Matches[0].LineTruncated {
		t.Fatalf("64 KiB 长行应可检索，err=%v", err)
	}
}

// TestDiffBaseline 检查未修改时为空，并确保基准来自 Prepare 时的快照。
// 后续源文件或副本的变化不能重置基准，修改范围保护仍由 Diff 再次校验。
func TestDiffBaseline(t *testing.T) {
	source := makeSource(t)
	if err := os.WriteFile(filepath.Join(source, "range.go"), []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	w := prepare(t, source, t.TempDir())
	if patch, err := w.Diff(context.Background()); err != nil || patch != "" {
		t.Fatalf("未修改时应为空，patch=%q err=%v", patch, err)
	}
	// 同时改变源和工作副本，预期减号行仍然是第一次复制时的 old。
	if err := os.WriteFile(filepath.Join(source, "range.go"), []byte("source-changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile(context.Background(), "range.go", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	patch, err := w.Diff(context.Background())
	want := "--- a/range.go\n+++ b/range.go\n@@ -1,1 +1,1 @@\n-old\n+new\n"
	if err != nil || patch != want {
		t.Fatalf("快照 Diff 错误：%q err=%v", patch, err)
	}
	// 再次修改后仍比较初始 old，不能把上一次生成 Diff 当成新的原始版本。
	if err := w.WriteFile(context.Background(), "range.go", []byte("newer\n")); err != nil {
		t.Fatal(err)
	}
	patch, err = w.Diff(context.Background())
	if err != nil || !strings.Contains(patch, "-old\n+newer\n") {
		t.Fatalf("基准发生漂移：%q err=%v", patch, err)
	}
}

// TestDiffRejectsOutOfScope 模拟绕过受控写入接口后修改保护文件或添加文件。
// Diff 必须独立发现异常并拒绝交付 Patch，不能只依赖 WriteFile 曾经检查过权限。
func TestDiffRejectsOutOfScope(t *testing.T) {
	for _, name := range []string{"go.mod", "range_test.go", ".env"} {
		t.Run(name, func(t *testing.T) {
			w := prepare(t, makeSource(t), t.TempDir())
			if err := os.WriteFile(filepath.Join(w.Dir(), name), []byte("changed"), 0644); err != nil {
				t.Fatal(err)
			}
			want := workspace.ErrProtectedChanged
			if name == ".env" {
				want = workspace.ErrUnexpectedFiles
			}
			patch, err := w.Diff(context.Background())
			if !errors.Is(err, want) || patch != "" {
				t.Fatalf("修改范围应拒绝，patch=%q err=%v", patch, err)
			}
		})
	}
}

// TestDiffPatchRoundTrip 用 Git 在另一个临时目录校验并应用 Patch，再比较实际文件字节。
// Git 只处理测试样本，命令与参数由测试代码固定，且共享 5 秒超时，不执行任何目标 Go 代码。
func TestDiffPatchRoundTrip(t *testing.T) {
	cases := []struct{ name, before, after string }{
		{"lines", "old\n", "new\n"},
		{"no-final-newline", "old", "new"},
		{"add-final-newline", "old", "old\n"},
		{"remove-final-newline", "old\n", "old"},
		{"empty-before", "", "new\n"},
		{"empty-after", "old\n", ""},
		{"crlf", "old\r\n", "new\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 在 Prepare 之前设置原始字节，创建后仅用受控接口修改副本。
			source := makeSource(t)
			if err := os.WriteFile(filepath.Join(source, "range.go"), []byte(tc.before), 0644); err != nil {
				t.Fatal(err)
			}
			w := prepare(t, source, t.TempDir())
			if err := w.WriteFile(context.Background(), "range.go", []byte(tc.after)); err != nil {
				t.Fatal(err)
			}
			patch, err := w.Diff(context.Background())
			if err != nil || patch == "" {
				t.Fatalf("应生成完整 Patch，err=%v", err)
			}
			// 独立的 Patch 消费端只操作新建临时目录，不能应用到真实示例或当前工作区。
			verifyDir := t.TempDir()
			verifyFile := filepath.Join(verifyDir, "range.go")
			patchFile := filepath.Join(t.TempDir(), "change.patch")
			if err := os.WriteFile(verifyFile, []byte(tc.before), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(patchFile, []byte(patch), 0644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, args := range [][]string{
				{"apply", "--check", "--whitespace=nowarn", "--", patchFile},
				{"apply", "--whitespace=nowarn", "--", patchFile},
			} {
				// 只对当前 Git 命令关闭自动换行转换，避免用户全局配置改变测试文件字节。
				// 不修改全局 Git 配置，CRLF 样本也应按 Patch 自身的字节保留。
				gitArgs := append([]string{"-c", "core.autocrlf=false"}, args...)
				cmd := exec.CommandContext(ctx, "git", gitArgs...)
				cmd.Dir = verifyDir
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("Git Patch 校验失败：%v\n%s", err, output)
				}
			}
			actual, err := os.ReadFile(verifyFile)
			if err != nil || !bytes.Equal(actual, []byte(tc.after)) {
				t.Fatalf("Patch 应用结果错误：%q err=%v", actual, err)
			}
		})
	}
}

// TestDiffOutputAndTextLimits 使用独立的字面头部计算边界，检查完整 Patch 的 8 KiB 上限。
// 同时拒绝非 UTF-8 或含 NUL 的目标文本，包括内容相同但编码不合法的情况。
func TestDiffOutputAndTextLimits(t *testing.T) {
	// 空源到一条无末尾换行的新增行，下面的头部与标记是手工写出的期望格式。
	header := "--- a/range.go\n+++ b/range.go\n@@ -0,0 +1,1 @@\n"
	overhead := len(header) + len("+\n\\ No newline at end of file\n")
	for _, size := range []int{8 * 1024, 8*1024 + 1} {
		source := makeSource(t)
		if err := os.WriteFile(filepath.Join(source, "range.go"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		w := prepare(t, source, t.TempDir())
		if err := w.WriteFile(context.Background(), "range.go", []byte(strings.Repeat("x", size-overhead))); err != nil {
			t.Fatal(err)
		}
		patch, err := w.Diff(context.Background())
		if size == 8*1024 && (err != nil || len(patch) != size) {
			t.Fatalf("恰好 8 KiB 应成功，len=%d err=%v", len(patch), err)
		}
		if size > 8*1024 && (!errors.Is(err, workspace.ErrOutputLimit) || patch != "") {
			t.Fatalf("超限不能输出半份 Patch，err=%v", err)
		}
	}
	for _, content := range [][]byte{{0xff}, {'x', 0}} {
		source := makeSource(t)
		if err := os.WriteFile(filepath.Join(source, "range.go"), content, 0644); err != nil {
			t.Fatal(err)
		}
		w := prepare(t, source, t.TempDir())
		if patch, err := w.Diff(context.Background()); !errors.Is(err, workspace.ErrInvalidText) || patch != "" {
			t.Fatalf("非法文本应拒绝，err=%v", err)
		}
		if _, err := w.Search(context.Background(), "x"); !errors.Is(err, workspace.ErrInvalidText) {
			t.Fatalf("检索非法文本应拒绝，err=%v", err)
		}
	}
}

// TestInspectCancelledAndClosed 检查三个新入口都遵守预取消和关闭后的拒绝契约。
// 有效关键字和正常目录排除其他失败原因，测试不声称覆盖运行中取消。
func TestInspectCancelledAndClosed(t *testing.T) {
	w := prepare(t, makeSource(t), t.TempDir())
	calls := []func(context.Context) error{
		func(ctx context.Context) error { _, err := w.ListFiles(ctx); return err },
		func(ctx context.Context) error { _, err := w.Search(ctx, "max"); return err },
		func(ctx context.Context) error { _, err := w.Diff(ctx); return err },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, call := range calls {
		if err := call(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("预取消应拒绝，err=%v", err)
		}
	}
	// 先显式关闭，再用正常 context 调用，确认错误来自已关闭资源。
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Fatal("关闭后不应继续列表、检索或生成 Diff")
		}
	}
}

```

## 2. 用接口骨架观察失败

创建 D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\inspect.go，暂时填入下面的接口骨架。它让测试先编译，再因具体功能未实现而失败。这个步骤不需要单独回传或等待确认。

```go

// Package workspace 的检索与 Diff 接口暂用骨架观察测试失败。
// 完整实现会在确认测试因功能未实现而失败之后替换。
package workspace

import (
	"context"
	"errors"
)

// 查询、匹配、预览及输出使用固定上限，完整实现必须落实这些限制。
const (
	MaxSearchQueryBytes = 128
	MaxSearchMatches = 20
	MaxSearchLineBytes = 256
	MaxToolOutputBytes = 8 * 1024
)

// 各错误类别是测试与调用者约定的契约，避免根据报错字符串判断失败。
var (
	ErrInvalidQuery = errors.New("workspace: invalid keyword")
	ErrUnexpectedFiles = errors.New("workspace: unexpected file layout")
	ErrInvalidText = errors.New("workspace: UTF-8 text without NUL required")
	ErrProtectedChanged = errors.New("workspace: protected file changed")
	ErrMissingBaseline = errors.New("workspace: original snapshot missing")
	ErrOutputLimit = errors.New("workspace: output exceeds 8 KiB")
)

// SearchMatch 描述一个匹配行及预览是否缩短，行号从 1 开始。
type SearchMatch struct {
	File string `json:"file"`
	Line int `json:"line"`
	Text string `json:"text"`
	LineTruncated bool `json:"line_truncated"`
}

// SearchResult 保存匹配数组，Truncated 表示有整条匹配被预算省略。
type SearchResult struct {
	Matches []SearchMatch `json:"matches"`
	Truncated bool `json:"truncated"`
}

// ListFiles 暂返回未实现错误，先让测试证明实际列表契约尚未满足。
func (w *Workspace) ListFiles(ctx context.Context) ([]string, error) {
	return nil, errors.New("ListFiles 尚未实现")
}

// Search 暂返回未实现错误，不提供任何文件检索结果。
func (w *Workspace) Search(ctx context.Context, keyword string) (SearchResult, error) {
	return SearchResult{}, errors.New("Search 尚未实现")
}

// Diff 暂返回未实现错误，下一步接入私有基准与完整 Patch 生成。
func (w *Workspace) Diff(ctx context.Context) (string, error) {
	return "", errors.New("Diff 尚未实现")
}

```

```powershell

go test ./internal/workspace -run '^(TestList|TestSearch|TestDiff|TestInspect)' -count=1

```

预期 FAIL，原因应包含 ListFiles、Search 或 Diff 尚未实现。若是重复定义、缺少原辅助函数或其他语法错误，先修正文件位置或粘贴错误，再继续。

## 3. 在已有 Workspace 中保存初始字节

修改 D:\Users\ZhuanZ1\Desktop\Eino_project\internal\workspace\workspace.go 的四处内容，其余逻辑保留。

第一处：把 Workspace 结构体替换为以下定义，保留原有函数注释和方法：

```go

// Workspace 持有任务副本及原始内容快照，文件接口不能修改内存中的比较基准。
// 生成 Diff 时先完整检查修改范围，使用结束后由调用者 Close 释放根句柄。
type Workspace struct {
    dir  string
    root *os.Root
    // original 保存 Prepare 时的原始字节，不放在可修改的工作区内。
    // ReadFile 返回新读取内容，WriteFile 只操作磁盘，都不会暴露或重置这个基准。
    original map[string][]byte
}

```

第二处：在 Prepare 的三个文件复制循环之前新增下面的变量。for 循环保留原有内容：

```go

// 三份快照内容的总长度最多 3×64 KiB，不含 map 与切片容量开销。
// 快照随 Workspace 生命周期保留，不重新读取变化后的源目录。
original := make(map[string][]byte, 3)

```

第三处：找到复制循环中的 errors.Join(writeErr, file.Close()) 检查，在它后面、仍在循环内部新增赋值，改成下面这段：

```go

// 写入和关闭均成功后保留本轮读取的原始字节，创建失败不交付部分快照。
// 文件工具后续重新读写磁盘，不会修改这个 data 切片。
if err := errors.Join(writeErr, file.Close()); err != nil {
    return nil, err
}
original[name] = data

```

第四处：将 Prepare 最后的返回语句替换为以下内容，前面的 ctx 检查与 keep = true 保留：

```go

// 成功时把根句柄和初始字节一起交给调用者；原始目录的句柄仍由本函数释放。
// 该对象生成 Diff 后再 Close，基准不会随每次修改或生成 Diff 漂移。
return &Workspace{
    dir: filepath.Join(absoluteParent, taskName),
    root: taskRoot,
    original: original,
}, nil

```

## 4. 替换 inspect.go 为完整实现

用以下内容完整替换接口骨架。不要再保留重复的类型、错误变量或方法定义：

```go

// 本文件提供固定工作区的实际文件列表、字面关键词检索与受限 unified Diff。
// 所有能力均围绕已有根句柄和白名单展开，不开放通用 Shell 或任意宿主路径。
package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// 检索同时约束输入、条数、预览和实际 JSON 大小，Diff 则必须保留完整 Patch。
// 限额是本批固定的执行策略，不交给模型自由设置。
const (
	MaxSearchQueryBytes = 128
	MaxSearchMatches    = 20
	MaxSearchLineBytes  = 256
	MaxToolOutputBytes  = 8 * 1024
)

// 错误类别区分参数、布局、文本、保护文件、基准缺失和输出预算问题。
// 调用者通过 errors.Is 判断类别，不需要匹配报错文字。
var (
	ErrInvalidQuery     = errors.New("workspace: invalid keyword")
	ErrUnexpectedFiles  = errors.New("workspace: unexpected file layout")
	ErrInvalidText      = errors.New("workspace: UTF-8 text without NUL required")
	ErrProtectedChanged = errors.New("workspace: protected file changed")
	ErrMissingBaseline  = errors.New("workspace: original snapshot missing")
	ErrOutputLimit      = errors.New("workspace: output exceeds 8 KiB")
)

// SearchMatch 表示一个匹配行，行号从 1 开始，同一行多次命中只返回一条。
// Text 是受限预览，LineTruncated 表示预览缩短，原文件内容未改变。
type SearchMatch struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Text          string `json:"text"`
	LineTruncated bool   `json:"line_truncated"`
}

// SearchResult 汇总按文件名与行号排列的匹配行，并标识是否省略了整条结果。
// Truncated 表示条数或 JSON 预算导致遗漏，与每行的 LineTruncated 分开。
type SearchResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// ListFiles 要求实际目录恰好含三个允许的普通文件，成功后返回排序名称。
// 最多读取四项：发现第四项已足以拒绝异常布局，无需枚举所有不允许的文件。
func (w *Workspace) ListFiles(ctx context.Context) ([]string, error) {
	// 在预取消时停止；通过根句柄打开当前目录，关闭后的实例也会在这里失败。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := w.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	// 三项并遇到 EOF 是合法情况，其他数量立即拒绝，不回传额外文件内容。
	entries, err := dir.ReadDir(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) != 3 {
		return nil, ErrUnexpectedFiles
	}

	// 数量相同仍可能有替换项，所以逐个检查原始名称与实际文件类型。
	// 名称唯一且都属于三项白名单，就能确定实际集合与允许集合一致。
	names := make([]string, 0, 3)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := checkName(entry.Name(), false); err != nil {
			return nil, ErrUnexpectedFiles
		}
		info, err := w.root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ErrNotRegular
		}
		names = append(names, entry.Name())
	}
	// 排序使输出与后续检索稳定，不依赖操作系统目录枚举顺序。
	sort.Strings(names)
	return names, ctx.Err()
}

// Search 使用区分大小写的字面关键字检索三个文件，不使用正则或递归搜索。
// 复用 ReadFile 的大小限制，并控制命中条数、预览字节和实际 JSON 输出。
func (w *Workspace) Search(ctx context.Context, keyword string) (SearchResult, error) {
	// 查询只接受单行 UTF-8 文本；空格可检索，空串、跨行和 NUL 则拒绝。
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}
	if keyword == "" || len(keyword) > MaxSearchQueryBytes || !utf8.ValidString(keyword) || strings.ContainsAny(keyword, "\r\n\x00") {
		return SearchResult{}, ErrInvalidQuery
	}
	names, err := w.ListFiles(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{Matches: make([]SearchMatch, 0)}

	// 每个文件先受限读取，再检查编码；Scanner 按行扫描并去除 CRLF 的 CR。
	// token 上限加一，允许恰好 64 KiB 且没有末尾换行的一整行。
	for _, name := range names {
		data, err := w.ReadFile(ctx, name)
		if err != nil {
			return SearchResult{}, err
		}
		if err := checkText(data); err != nil {
			return SearchResult{}, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 4096), MaxFileBytes+1)
		lineNumber := 0
		for scanner.Scan() {
			// 循环中传播取消；先检索完整行，再缩短显示预览，避免漏掉行尾关键字。
			lineNumber++
			if err := ctx.Err(); err != nil {
				return SearchResult{}, err
			}
			line := scanner.Text()
			if !strings.Contains(line, keyword) {
				continue
			}
			if len(result.Matches) == MaxSearchMatches {
				result.Truncated = true
				return result, ctx.Err()
			}
			preview, shortened := previewLine(line)
			result.Matches = append(result.Matches, SearchMatch{File: name, Line: lineNumber, Text: preview, LineTruncated: shortened})

			// JSON 转义会放大文本，因此按实际编码结果检查输出预算。
			// 超限时撤回最后一整条匹配，保证结果完整、可解析并明确标记遗漏。
			encoded, err := json.Marshal(result)
			if err != nil {
				return SearchResult{}, err
			}
			if len(encoded) > MaxToolOutputBytes {
				result.Matches = result.Matches[:len(result.Matches)-1]
				result.Truncated = true
				return result, ctx.Err()
			}
		}
		if err := scanner.Err(); err != nil {
			return SearchResult{}, err
		}
	}
	return result, ctx.Err()
}

// checkText 限定文本接口只处理合法 UTF-8 且不含 NUL 的内容。
// ReadFile 仍是受限字节接口，文本约束由检索和 Patch 生成负责。
func checkText(data []byte) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return ErrInvalidText
	}
	return nil
}

// previewLine 将长行限制在 256 字节，并退回到完整 UTF-8 字符边界。
// 输入已经通过 checkText，最多退回当前被切断字符的几个字节。
func previewLine(line string) (string, bool) {
	if len(line) <= MaxSearchLineBytes {
		return line, false
	}
	preview := line[:MaxSearchLineBytes]
	for !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}
	return preview, true
}

// Diff 对比 Prepare 时保管的快照，先验证文件集合和两个保护文件内容。
// 只为 range.go 生成完整 Patch；超限报错，不能把半份 Patch 当成可应用结果。
func (w *Workspace) Diff(ctx context.Context) (string, error) {
	names, err := w.ListFiles(ctx)
	if err != nil {
		return "", err
	}
	var before, after []byte
	// 集合检查发现不了同名内容替换，因此再次比对私有原始快照。
	// 这项检查独立于 WriteFile，能发现测试模拟的绕过接口写入。
	for _, name := range names {
		original, ok := w.original[name]
		if !ok {
			return "", ErrMissingBaseline
		}
		current, err := w.ReadFile(ctx, name)
		if err != nil {
			return "", err
		}
		if name == "range.go" {
			before, after = original, current
		} else if !bytes.Equal(original, current) {
			return "", fmt.Errorf("%w: %s", ErrProtectedChanged, name)
		}
	}
	// 即使内容相同也检查文本范围，避免以“没有变化”绕过编码约束。
	if err := checkText(before); err != nil {
		return "", err
	}
	if err := checkText(after); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if bytes.Equal(before, after) {
		return "", nil
	}

	// 固定小示例采用整文件替换的单个 hunk，保持合法格式而不追求最小差异。
	// 新旧字节之和已超限时立即停止，避免构建明显过大的输出。
	if len(before)+len(after) > MaxToolOutputBytes {
		return "", ErrOutputLimit
	}
	oldStart, oldCount := lineSpan(before)
	newStart, newCount := lineSpan(after)
	var patch strings.Builder
	fmt.Fprintf(&patch, "--- a/range.go\n+++ b/range.go\n@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	writePatchLines(&patch, '-', before)
	writePatchLines(&patch, '+', after)
	if patch.Len() > MaxToolOutputBytes {
		return "", ErrOutputLimit
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return patch.String(), nil
}

// lineSpan 计算整文件 hunk 的起点与行数：空文件使用 0,0，有内容时从 1 开始。
// 末尾没有换行的最后一行也要计数，不能只统计换行字节。
func lineSpan(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	count := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		count++
	}
	return 1, count
}

// writePatchLines 为原始行加删除或新增前缀，保留原始行尾，包括 CRLF。
// 无末尾换行时追加标准标记，使消费者恢复准确的目标字节，而非强行加换行。
func writePatchLines(patch *strings.Builder, prefix byte, data []byte) {
	if len(data) == 0 {
		return
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		patch.WriteByte(prefix)
		patch.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			patch.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

```

## 5. 集中验收和提交

在项目根目录 PowerShell 执行：

```powershell

gofmt -w internal/workspace/workspace.go internal/workspace/inspect.go internal/workspace/inspect_test.go
go test ./internal/workspace -count=1 -v
go vet ./internal/workspace
go test ./... -count=1
git diff --check
git status --short

```

预期 02 的所有新增测试通过，01 的原有测试继续通过；已有两个 Windows 链接子测试仍可能 SKIP，保留原因。目标示例模块不应出现在执行测试的包列表中。TestDiffPatchRoundTrip 只在测试临时目录调用 Git，不执行目标 Go 测试，不请求模型 API。

常见问题：

- ErrMissingBaseline：检查第三步的字段、map 初始化、循环内赋值和返回对象是否全部接入。
- undefined: makeSource 或 prepare：inspect_test.go 必须与已有 workspace_test.go 在同一个目录，包名同为 workspace_test。
- redeclared 或方法重复：用完整实现覆盖接口骨架，不要把它们同时保留在多个文件里。
- executable file not found：确认当前 PowerShell 的 Git 可用；不要把 Patch 校验改成“只检查输出字符串”。
- Git 应用结果出现额外 CRLF：确认测试中的 -c core.autocrlf=false 没有漏掉；它只固定当前命令，不更改全局配置。
- ErrOutputLimit：本批整文件 hunk 适用于固定小示例，8 KiB 超限属于明确失败，不应删除限额或交付半份 Patch。

通过后提交本批涉及的文件：

```powershell

git add -- internal/workspace/workspace.go internal/workspace/inspect.go internal/workspace/inspect_test.go docs/progress.md docs/phase2-02-inspect-diff.md
git diff --cached --check
git diff --cached --stat
git commit -m "feat: add bounded workspace search and diff"
git log -1 --oneline

```

集中回传详细工作区测试、主模块测试、go vet 是否无错误，以及最后一条 commit。无需重新提供密钥、调用付费模型或复验 Docker 版本。失败时回传最早错误并保留失败断言，不先提交。

## 本批机制与验证范围

- 写入权限与结果验收分开：WriteFile 在入口拒绝保护文件，Diff 在出口重新检查文件集合和保护文件字节。后者能发现可信测试模拟的接口绕过，但仍假定宿主没有并发篡改；它不代替容器约束。
- 原始基准属于外层程序：快照放在私有内存，不放进工作区，不通过文件工具提供写入能力；对象只在完整创建后交付。之后修改源目录也不改变已有任务的基准。
- 检索提供文件、行号和受限预览，后续 Agent 可以按需读取，不必一次把全部源码放进上下文。预览缩短与省略匹配行是两种状态，不能混为一谈。
- JSON 字节上限不是 Token 上限。这里限制实际序列化输出；模型步数、调用、总时限和可靠用量管理仍需后续接入。
- ScanLines 支持 LF/CRLF，Buffer 上限需要显式设置以接受合法长行。[Go 1.27.1 bufio 文档](https://pkg.go.dev/bufio@go1.27.1#Scanner.Buffer)
- 有界目录枚举使用 File.ReadDir(4)，而非 os.ReadDir 先读取全目录再截取。[Go 1.27.1 File.ReadDir](https://pkg.go.dev/os@go1.27.1#File.ReadDir)
- Patch 只是变更记录。Git --check 检查能否应用，临时应用再对比字节检查生成结果；这些测试不证明 Bug 已修复。[Git apply 官方文档](https://git-scm.com/docs/git-apply)

导师验证：本讲义候选在独立临时副本中已观察接口骨架导致的预期失败，接入实现后运行该副本完整 go test ./... 与 go vet ./... 通过（沿用 01 的两个 Windows 链接 SKIP）；随后补充同数量错误名称测试并单独运行通过。真实项目尚未接入本批，所以仍需你亲手实现与集中验收。

复盘题：如果受控 WriteFile 已经禁止修改测试，为什么 Diff 仍然要重新验证测试文件和文件集合？请结合“入口权限”和“出口验收”的不同职责解释。

本批通过后进入最小 Docker 测试沙箱和执行预算，再首次复现目标 Bug；检索与 Diff 目前是 Go 受控能力，后续与沙箱一起接入有界模型工具调用闭环。
