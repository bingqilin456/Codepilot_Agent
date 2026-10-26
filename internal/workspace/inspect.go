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
