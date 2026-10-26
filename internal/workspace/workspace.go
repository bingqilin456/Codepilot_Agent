// Package workspace 为固定授权的 boundary 示例创建独立副本，并提供受控文件读写。
// 文件名白名单负责业务权限，os.Root 负责实际目录访问边界；目标代码执行另需 Docker 沙箱。
// 当前按串行文件操作设计，源目录和父目录由外层程序配置，假定宿主进程不会并发篡改它们。
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

// MaxFileBytes 限制单个文件可接受的内容大小，避免无界读取和过大的修改输入。
// 读取最多探测上限再加一个字节，返回内容不超过此值；写入必须先检查大小再修改文件。
const MaxFileBytes = 64 * 1024

// 错误类别供调用者区分权限拒绝、文件超限和不支持的文件类型。
// 调用者应使用 errors.Is 检查类别，保留 fmt.Errorf 的 %w 包装和 errors.Join 中的错误信息。
var (
	// ErrDenied 表示文件名或读写权限不符合当前固定示例的白名单。
	ErrDenied = errors.New("workspace: file access denied")
	// ErrTooLarge 表示输入或实际读取的文件内容超过单文件上限。
	ErrTooLarge = errors.New("workspace: file exceeds 64 KiB")
	// ErrNotRegular 表示该位置是目录、符号链接或其他非普通文件。
	ErrNotRegular = errors.New("workspace: regular file required")
)

// Workspace 持有任务副本及原始内容快照，文件接口不能修改内存中的比较基准。
// 生成 Diff 时先完整检查修改范围，使用结束后由调用者 Close 释放根句柄。
type Workspace struct {
	dir  string
	root *os.Root
	// original 保存 Prepare 时的原始字节，不放在可修改的工作区内。
	// ReadFile 返回新读取内容，WriteFile 只操作磁盘，都不会暴露或重置这个基准。
	original map[string][]byte
}

// Dir 返回工作区的绝对路径，便于外层程序检查目录和组织后续执行环境。
// 获取路径不会打开或修改文件；业务文件操作仍应经过 ReadFile 和 WriteFile。
func (w *Workspace) Dir() string { return w.dir }

// Close 释放目录句柄，保留工作区内容供后续 Diff 和结果查询使用。
// 调用后该句柄不能继续读写；删除目录前应先关闭句柄，Windows 尤其依赖这个顺序。
func (w *Workspace) Close() error { return w.root.Close() }

// Prepare 从固定授权的 boundary 示例复制三个普通文件，返回独立工作区及其目录句柄。
// sourceDir 和 parentDir 只能来自外层程序的可信配置，不能让模型任意选择宿主路径。
// 成功时保留副本并交给调用者管理；失败时清理本次生成的任务目录，不删除源目录。
func Prepare(ctx context.Context, sourceDir, parentDir string) (_ *Workspace, retErr error) {
	// 在任何目录创建或文件访问之前检查取消，避免已取消任务产生工作区副作用。
	// Context 只在显式检查处生效，不能自动中断普通文件系统调用。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 打开源目录的根句柄，后面的复制读取统一相对于这个目录执行。
	// 创建结束后源句柄不再使用，因此由本函数的 defer 释放；返回的任务句柄另有生命周期。
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}
	defer source.Close()

	// 先确定父目录的绝对路径并打开父根句柄，保证返回的 Dir 不依赖后续工作目录变化。
	// 任务目录的创建与失败清理都通过该父句柄完成，将操作限定在配置的父目录内。
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

	// 每次创建由程序生成随机名称的子目录，防止多个任务共享同一份可修改代码。
	// Mkdir 遇到同名目录会直接失败，不覆盖或复用它；新文件随后通过任务根句柄创建。
	taskName := "task-" + rand.Text()
	if err := parent.Mkdir(taskName, 0755); err != nil {
		return nil, err
	}
	// keep 只在完整复制成功时设为 true；注册此 defer 后、keep 置 true 前的失败返回会触发清理。
	// 该 defer 后于 parent.Close 注册，所以先关闭任务句柄、删除子目录，再关闭父句柄。
	// retErr 是命名返回值，清理错误通过 errors.Join 合并进去，保留原始失败原因。
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
	// 在父目录边界内打开新任务的根句柄，后续副本文件只在这个任务目录下创建。
	// 如果打开失败，已经注册的清理仍会删除刚创建的空目录。
	taskRoot, err = parent.OpenRoot(taskName)
	if err != nil {
		return nil, err
	}

	// 三份快照内容的总长度最多 3×64 KiB，不含 map 与切片容量开销。
	// 快照随 Workspace 生命周期保留，不重新读取变化后的源目录。
	original := make(map[string][]byte, 3)

	// 只按固定清单复制三个文件，不递归导入 .env、.git 或源目录中的其他内容。
	// readRegular 同时检查取消、普通文件类型与读取上限；复制顺序也便于测试部分失败后的清理。
	for _, name := range []string{"go.mod", "range.go", "range_test.go"} {
		data, err := readRegular(ctx, source, name)
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", name, err)
		}

		// 读取完成后再检查取消，尽量避免任务已经取消却继续创建或写入副本。
		// 如果此时停止，前面已经复制的文件由本函数统一清理。
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// 每个副本创建独立的新文件，不能用硬链接共享源文件的内容。
		// O_CREATE|O_EXCL 要求文件此前不存在，避免误覆盖任务目录里已有的同名文件。
		file, err := taskRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return nil, err
		}
		// 核对实际写入字节数，并在本轮复制中立即关闭文件，避免循环积累打开的句柄。
		// 写入与关闭的错误一起返回；任一失败都会使整个工作区创建失败并触发清理。
		n, writeErr := file.Write(data)
		if writeErr == nil && n != len(data) {
			writeErr = io.ErrShortWrite
		}

		// 写入和关闭均成功后保留本轮读取的原始字节，创建失败不交付部分快照。
		// 文件工具后续重新读写磁盘，不会修改这个 data 切片。
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return nil, err
		}
		original[name] = data

	}

	// 完成全部复制后最后检查一次取消，只有仍然有效的任务才交付工作区。
	// keep=true 让清理保留副本，同时把任务根句柄的关闭责任移交给调用者。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true

	// 成功时把根句柄和初始字节一起交给调用者；原始目录的句柄仍由本函数释放。
	// 该对象生成 Diff 后再 Close，基准不会随每次修改或生成 Diff 漂移。
	return &Workspace{
		dir:      filepath.Join(absoluteParent, taskName),
		root:     taskRoot,
		original: original,
	}, nil

}

// checkName 按原始文件名判定业务权限：三个文件可读，只有 range.go 可写。
// 不先 Clean 或转换大小写，拒绝路径穿越、绝对路径、Windows ADS 以及名称别名。
// 名字检查不能判断磁盘上是否存在链接，文件类型和目录边界还需要在实际访问时检查。
func checkName(name string, write bool) error {
	if name == "range.go" || (!write && (name == "go.mod" || name == "range_test.go")) {
		return nil
	}
	return ErrDenied
}

// ReadFile 返回白名单内普通文件的内容，拒绝越权名称、非普通文件和超过上限的内容。
// 先判定业务权限，再交给共用读取函数处理取消、目录边界及文件大小。
func (w *Workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if err := checkName(name, false); err != nil {
		return nil, err
	}
	return readRegular(ctx, w.root, name)
}

// readRegular 在给定根句柄内读取普通文件，供源文件复制和工作区读取共同使用。
// 它不决定文件名白名单：调用方必须先限定名称；本函数负责类型、大小和取消检查。
func readRegular(ctx context.Context, root *os.Root, name string) ([]byte, error) {
	// 已取消的读取在开始文件访问前直接返回，使调用者能够按 context 错误结束任务。
	// 调用方传入同一个任务 context 时，复制和读取会共享该任务的取消信号。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Lstat 检查路径位置本身，不跟随最后一级符号链接，因此可以拒绝链接和目录。
	// 之后仍通过 root.Open 打开文件，实际访问的目录边界不能仅依赖这次预检查。
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}

	// 在根目录约束下打开文件，并通过已打开句柄再次检查实际对象是否是普通文件。
	// 这次检查针对句柄对应的对象；本函数结束时释放读取句柄。
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

	// 先利用文件元数据快速拒绝已经超限的文件，避免为明显过大的内容分配缓冲区。
	// 元数据只是读取前的观察，不能独自约束文件随后增长的情况。
	if info.Size() > MaxFileBytes {
		return nil, ErrTooLarge
	}

	// 实际最多读取上限加一个字节，区分“刚好达到上限”和“还有更多内容”。
	// 即使文件在元数据检查后变大，LimitReader 也会限制本次读取的数据量。
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, ErrTooLarge
	}

	// 读取期间可能收到取消信号，因此交付结果前再次检查任务是否有效。
	// 这里返回取消错误而不返回内容，但没有中断已经发生的文件读取。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// WriteFile 覆盖已有的 range.go，在修改前检查取消、业务权限、输入大小和文件类型。
// 被这些前置检查拒绝时不截断旧文件；命名返回值用于合并后续关闭文件的错误。
// 写入不是原子事务：实际 I/O 失败或写入完成后才发现取消时，文件内容可能已经改变。
func (w *Workspace) WriteFile(ctx context.Context, name string, data []byte) (retErr error) {
	// 先处理已取消任务和不允许的文件名，再判断内容大小，避免拒绝请求产生文件副作用。
	// 尤其不能先用 O_TRUNC 打开文件，再发现输入超过上限。
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName(name, true); err != nil {
		return err
	}
	if len(data) > MaxFileBytes {
		return ErrTooLarge
	}

	// 检查待写位置本身是普通文件，拒绝同名目录、链接或其他特殊文件。
	// 本接口只修改已有文件，不通过用户提供的名称创建新的目标文件。
	info, err := w.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	// 以只写方式打开，但不指定 O_TRUNC，保证打开成功本身不会清空旧内容。
	// 后面还需要检查实际文件对象和取消信号，全部通过后才开始修改。
	file, err := w.root.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	// 打开成功后的每条返回路径都关闭文件，并将关闭错误加入 retErr。
	// 对实际打开的对象再次检查类型，减少仅依据路径预检查造成的误判。
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotRegular
	}

	// 在第一个修改动作之前再次检查取消；只有前置检查全部通过，才清空已有内容。
	// 从 Truncate 成功开始就可能产生副作用，后面的失败不会自动恢复旧文件。
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	// 写入完整的新内容并检查短写，防止把只写入一部分的数据报告为成功。
	// 最后报告可能到达的取消信号；此时数据可能已写入，调用方不能将错误理解为自动回滚。
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return ctx.Err()
}
