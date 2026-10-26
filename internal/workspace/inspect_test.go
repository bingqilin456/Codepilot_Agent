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
