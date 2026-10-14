// Package workspace_test 从调用者视角验收工作区，只使用 workspace 的导出接口。
// 固定 Bug 示例在这里仅是字节模板，测试不会编译或执行示例中的 Go 代码。
// 可修改的源、工作区和根外样本都放在测试临时目录中，不改动真实示例或读取真实凭据。
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

/*
-----------------我学会的思想----------------

	思想一：即使只是简单的改个代码的操作，却有如此多的防御机制，看的我眼花缭乱
		比如：在makeSource中故意放了一个.env的假密码，来防止AI读取或修改
	又或者是TestPathPolicy中设置denied来专门防范通过符号链接指向外部系统文件
	写底层代码时，永远假设外部输入是恶意的，把防线建立在每一个系统调用和 API 入口处

	思想二：建立独立副本来隔离副作用
		就像我们在和大模型对话中，我们习惯新建对话来隔离与之前的任务进行干扰
	一个修复任务应只影响自己的工作区，原始仓库和其他任务保持独立。比如makeSource中
	为了让后续的异常测试不破坏真实示例，程序会在系统临时目录（t.TempDir()）中重建一份源数据。

	思想三：资源必须有明确的所有者和生命周期。
		代码不仅要“能跑”，还要能被安全地控制（取消、超时）和精确地被验证（测试定义契约）。
	任何需要长期存活或被多方调用的组件，必须暴露明确的生命周期接口（Init、Close、Context 透传）。
    同时，测试不是用来“证明代码能跑”的，而是用来“规定代码行为边界”的。
*/

// makeSource 把固定示例复制到可破坏的临时源，后面的异常测试不修改真实示例。
// 这是第一轮复制；后续 Prepare 再从临时源创建工作副本，测试可分别检查两层是否独立。
// 临时源额外放入假 .env，便于发现复制是否带入白名单之外的文件，不使用真实密钥。
func makeSource(t *testing.T) string {
	// 标记为辅助函数后，Fatal 的定位会略过这一层，指向调用它的测试。
	// 若准备样本失败，后续断言已没有可靠前提，因此停止当前测试。
	t.Helper()

	// 为本次测试创建独立的源目录；测试框架在测试及其子测试结束后自动清理。
	// 异常测试可以删除或替换这里的文件，固定示例仍保持原样。
	source := t.TempDir()

	// go test 运行本包测试时以 internal/workspace 为工作目录，因此向上两层找到项目模板。
	// 这里只定位并读取文件字节，不导入示例模块，也不执行其中的测试。
	fixture := filepath.Join("..", "..", "testdata", "bugs", "boundary")

	// 只把三个约定文件读出后重新写入临时源，形成可被异常测试修改的样本。
	// 测试准备使用可信的宿主文件 API；这些能力不会因此开放给模型。
	for _, name := range []string{"go.mod", "range.go", "range_test.go"} {
		data, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 用固定假内容模拟凭据文件，后续应证明它没有进入工作副本。
	// 文件名仅用于权限测试；这里不读取项目中可能存在的真实 .env。
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("FAKE_TEST_SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

// prepare 为需要成功工作区的测试统一调用 Prepare，减少重复的准备与清理代码。
// source 是临时源，parent 是临时父目录；返回的 task-* 副本位于 parent 内。
// 成功后把关闭动作交给当前测试生命周期，保证返回的工作区在断言期间仍可使用。
func prepare(t *testing.T, source, parent string) *workspace.Workspace {
	// 准备失败时定位到调用测试并停止；不能让后续断言继续使用无效的工作区。
	t.Helper()
	w, err := workspace.Prepare(context.Background(), source, parent)
	if err != nil {
		t.Fatal(err)
	}

	// Cleanup 在当前测试及其子测试结束后按注册逆序执行，先关闭句柄再清理临时目录。
	// 不能在本辅助函数中使用 defer w.Close，否则函数返回时工作区就已关闭。
	// 这里只安排关闭并忽略其返回错误；Close 保留文件，目录删除由 TempDir 负责。
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestCopyIsIndependent 验证复制完整、没有夹带文件，并且不同工作区相互独立。
// 修改第一份副本后，同时检查写入结果、源文件和第二份副本，避免只验证返回成功。
// 写入内容仅服务于文件接口验收，不要求它继续构成可通过目标测试的 Go 项目。
func TestCopyIsIndependent(t *testing.T) {
	// 用同一个临时源连续创建两个副本，源和副本父目录分开，便于观察复制边界。
	source, parent := makeSource(t), t.TempDir()
	first := prepare(t, source, parent)
	second := prepare(t, source, parent)

	// 不同任务不能复用同一个目录；第一份副本应直接位于约定的父目录内。
	// 前提失败时后续内容比较意义有限，因此用 Fatal 停止当前测试。
	if first.Dir() == second.Dir() || filepath.Dir(first.Dir()) != parent {
		t.Fatal("工作区必须是父目录下不同的独立目录")
	}

	// 文件总数与三个允许文件的内容检查结合，验证仅复制白名单且字节未丢失。
	// 临时源中的假 .env 不能被复制进来，错误时保留实际目录项供定位。
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

	// 保存修改前的源内容，再通过受控 WriteFile 改第一份副本并读回确认。
	// 只收到 nil 错误不足以证明写入有效，所以还要比较实际字节。
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

	// 同一修改不得传播到源文件或另一份副本，防止硬链接或共享文件造成连带修改。
	// 两处都用修改前的字节作基准，断言操作副作用只发生在第一份工作区。
	untouched, err := os.ReadFile(filepath.Join(source, "range.go"))
	if err != nil || !bytes.Equal(untouched, original) {
		t.Fatalf("源文件被改变，err=%v", err)
	}
	untouched, err = second.ReadFile(context.Background(), "range.go")
	if err != nil || !bytes.Equal(untouched, original) {
		t.Fatalf("另一工作区被改变，err=%v", err)
	}
}

// TestPathPolicy 检查权限规则是否真正执行，覆盖非法文件名与只读文件两类边界。
// 文件名必须原样匹配，不能把越界或别名路径规范化成允许名称后继续访问。
// 对禁止修改的文件同时检查错误类别和内容不变，确保拒绝发生在副作用之前。
func TestPathPolicy(t *testing.T) {
	// 先建立可用工作区，再集中给出路径穿越、绝对路径与 Windows 路径别名。
	// 大小写变体、ADS 和凭据文件名都应被业务白名单直接拒绝。
	w := prepare(t, makeSource(t), t.TempDir())
	denied := []string{
		"", "../range.go", "./range.go", "sub/../range.go",
		"/etc/passwd", "D:/secret.txt", "D:\\secret.txt",
		"sub\\range.go", ".env", "NUL", "range.go:secret", "RANGE.GO",
	}

	// 同一非法名称的读写都检查；Errorf 继续执行，以一次输出收集各个独立问题。
	// errors.Is 检查错误类别，允许实现通过包装错误补充上下文，不依赖报错文字。
	for _, name := range denied {
		if _, err := w.ReadFile(context.Background(), name); !errors.Is(err, workspace.ErrDenied) {
			t.Errorf("ReadFile(%q) 应拒绝，err=%v", name, err)
		}
		if err := w.WriteFile(context.Background(), name, []byte("x")); !errors.Is(err, workspace.ErrDenied) {
			t.Errorf("WriteFile(%q) 应拒绝，err=%v", name, err)
		}
	}

	// go.mod 与测试文件可以读取但不能写入：先留存内容，再尝试修改并读回。
	// 不能仅看到 ErrDenied 就算通过，还必须确认被拒绝的写入没有破坏原文件。
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

// TestFileSizeLimit 验证单文件上限包含恰好 64 KiB，但拒绝再多一个字节。
// 超限写入必须保持旧内容；可信测试另行造出超限文件，以检查读取端也执行限制。
// 本测试没有模拟 Stat 后文件增长，不能据此声称已覆盖 LimitReader 的增长防护分支。
func TestFileSizeLimit(t *testing.T) {
	// 先验证边界以内的正常读写，避免实现把所有较大输入一律拒绝却仍通过异常测试。
	// 使用固定字节而非可执行源码，因为这里验收的是文件大小契约。
	w := prepare(t, makeSource(t), t.TempDir())
	limit := bytes.Repeat([]byte("x"), 64*1024)
	if err := w.WriteFile(context.Background(), "range.go", limit); err != nil {
		t.Fatal(err)
	}
	got, err := w.ReadFile(context.Background(), "range.go")
	if err != nil || !bytes.Equal(got, limit) {
		t.Fatalf("64 KiB 应可读写，err=%v", err)
	}

	// 用 65,537 字节命中最小越界值，再读回原有 65,536 字节确认没有先截断文件。
	// 错误返回与副作用断言缺一不可，否则实现可能拒绝了请求却已破坏旧内容。
	oversized := bytes.Repeat([]byte("x"), 64*1024+1)
	if err := w.WriteFile(context.Background(), "range.go", oversized); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("超限写入应拒绝，err=%v", err)
	}
	got, err = w.ReadFile(context.Background(), "range.go")
	if err != nil || !bytes.Equal(got, limit) {
		t.Fatalf("超限写入不得截断旧文件，err=%v", err)
	}

	// 受控写入会阻止超限数据，因此可信测试用宿主 API 主动构造异常文件再验证读取。
	// 当前实现先由 Stat 拒绝这个已超限文件；MaxFileBytes+1 的限流用于另防读取中增长。
	// 这不是提供给模型的写入接口，也不会改变固定示例。
	if err := os.WriteFile(filepath.Join(w.Dir(), "range.go"), oversized, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadFile(context.Background(), "range.go"); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("超限读取应拒绝，err=%v", err)
	}
}

// TestCancelledContext 验证调用前已经取消时，创建和读写都返回 context.Canceled。
// 除了检查错误，还检查没有创建残留目录、取消写入没有改变原内容。
// 覆盖范围仅为预取消，不包含运行中取消、超时触发或写入后的事务回滚。
func TestCancelledContext(t *testing.T) {
	// 主动先取消同一个 context，再交给 Prepare，明确模拟操作开始前已失效的请求。
	// 临时源与父目录属于测试准备；这里检查的是 Prepare 是否又创建了任务子目录。
	source, parent := makeSource(t), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspace.Prepare(ctx, source, parent); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后不应创建工作区，err=%v", err)
	}

	// 在测试框架清理之前立即观察父目录，防止最终 TempDir 删除掩盖创建残留。
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("取消后不应留下目录，entries=%v err=%v", entries, err)
	}

	// 使用正常 context 创建可用工作区并保存旧内容，再把预取消 context 用于读写。
	// 取消过的 context 不能恢复；prepare 内部使用的是另一个 context.Background。
	w := prepare(t, source, parent)
	before, err := w.ReadFile(context.Background(), "range.go")
	if err != nil {
		t.Fatal(err)
	}

	// 对有效文件名测试取消契约，确保失败来自 context，而非文件权限或路径不存在。
	if _, err := w.ReadFile(ctx, "range.go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后不应读取，err=%v", err)
	}
	if err := w.WriteFile(ctx, "range.go", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后不应写入，err=%v", err)
	}

	// 用正常 context 读回，确认预取消写入在产生副作用之前已经被拒绝。
	// 不能推广为任意时刻取消都不修改文件，当前写入不是可回滚事务。
	after, err := w.ReadFile(context.Background(), "range.go")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("取消写入改变了文件，err=%v", err)
	}
}

// TestPrepareFailureCleansDirectory 验证源文件缺失、超限或类型错误时创建失败并立即清理。
// 故意破坏复制顺序中的第三个文件，让创建过程已经写过部分文件再遇到错误。
// 在 Prepare 返回后立刻检查父目录，验收的是失败回滚，不能依赖测试结束时的自动清理。
func TestPrepareFailureCleansDirectory(t *testing.T) {
	// 每种异常用独立子测试和临时样本，避免前一种破坏影响后一种错误类别。
	for _, failure := range []string{"missing", "oversized", "directory"} {
		t.Run(failure, func(t *testing.T) {
			// 仅删除临时源中的第三个文件；固定示例不变，前两个文件仍可被正常复制。
			source, parent := makeSource(t), t.TempDir()
			target := filepath.Join(source, "range_test.go")
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}

			// 删除后不重建就是 missing；其余分支分别造出超限普通文件与同名目录。
			// 三者分别触发不存在、大小和文件类型边界，不能用同一种错误笼统处理。
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

			// 直接调用 Prepare 观察失败结果，不通过要求成功的 prepare 辅助函数。
			// 如果意外成功，先关闭返回句柄再 Fatal，避免错误路径额外阻碍 Windows 清理。
			w, err := workspace.Prepare(context.Background(), source, parent)
			if err == nil {
				_ = w.Close()
				t.Fatal("异常源文件应使创建失败")
			}

			// errors.Is 保留对底层错误类别的检查，即使 Prepare 包装或合并了清理错误。
			// 后续任务管理可据错误类别区分输入问题，而不是匹配报错文本。
			if failure == "missing" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("缺失文件错误分类错误：%v", err)
			}
			if failure == "oversized" && !errors.Is(err, workspace.ErrTooLarge) {
				t.Fatalf("大小错误分类错误：%v", err)
			}
			if failure == "directory" && !errors.Is(err, workspace.ErrNotRegular) {
				t.Fatalf("文件类型错误分类错误：%v", err)
			}

			// 现在父目录必须已经为空，说明部分副本已由 Prepare 自己关闭句柄并删除。
			// 若等测试结束后再看，TempDir 的清理可能掩盖业务清理缺陷。
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("创建失败后留下部分工作区，entries=%v err=%v", entries, readErr)
			}
		})
	}
}

// replaceWithSymlink 用指向根外样本的链接替换合法文件名，供链接拒绝测试构造异常。
// os.Symlink 的第一个参数是链接目标 outside，第二个参数是新链接名 filename。
// 只对 Windows 已知的链接创建权限错误跳过当前子测试，其余错误仍应使测试失败。
func replaceWithSymlink(t *testing.T, filename, outside string) {
	// 失败位置指向调用测试；先移除临时文件，避免链接创建因名称已存在而失败。
	// 删除的是 filename 处的样本，不是 outside 指向的根外文件。
	t.Helper()
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}

	// 让允许的文件名成为通往根外的入口；这里依靠可信测试权限制造不可信文件状态。
	if err := os.Symlink(outside, filename); err != nil {
		// 权限不足时不能伪造通过：Skipf 标记当前子测试尚未验证并停止其执行。
		// 已注册的清理仍会执行；非 Windows 或其他创建错误走 Fatal，不被宽泛跳过。
		if runtime.GOOS == "windows" &&
			(errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(1314))) {
			t.Skipf("当前 Windows 无符号链接创建权限，本项尚未验证：%v", err)
		}
		t.Fatal(err)
	}
}

// TestRejectSymlink 分别验证源导入和工作区读写拒绝合法名称下的符号链接。
// 工作区分支还检查根外样本的字节不变，防止拒绝之前已经发生越界写入。
// 当前错误由 Lstat 的普通文件检查产生，本测试没有模拟检查后的并发替换或单独验证 Root 兜底。
func TestRejectSymlink(t *testing.T) {
	// 为两个权限入口分别构造独立样本，避免源导入失败阻止工作区读写测试。
	for _, location := range []string{"source", "workspace"} {
		t.Run(location, func(t *testing.T) {
			// 根外是相对于源或工作区的根目录，样本仍位于测试框架管理的临时目录。
			// 留存固定字节作为后续副作用检查基准，不接触宿主真实业务文件。
			source, parent := makeSource(t), t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside.go")
			original := []byte("outside-file-must-stay-unchanged")
			if err := os.WriteFile(outside, original, 0644); err != nil {
				t.Fatal(err)
			}

			// 源分支在复制前替换 range.go，Prepare 应因源不是普通文件而拒绝导入。
			// 这里只检查导入错误；成功工作区的读写拒绝交给另一子测试独立验收。
			if location == "source" {
				replaceWithSymlink(t, filepath.Join(source, "range.go"), outside)
				if _, err := workspace.Prepare(context.Background(), source, parent); !errors.Is(err, workspace.ErrNotRegular) {
					t.Fatalf("源链接应拒绝，err=%v", err)
				}
				return
			}

			// 工作区分支先成功复制，再替换副本的合法名称，检查读取和写入均拒绝链接。
			// 名称通过白名单不代表目标一定安全，实际文件类型仍须检查。
			w := prepare(t, source, parent)
			replaceWithSymlink(t, filepath.Join(w.Dir(), "range.go"), outside)
			if _, err := w.ReadFile(context.Background(), "range.go"); !errors.Is(err, workspace.ErrNotRegular) {
				t.Fatalf("工作区链接应拒绝读取，err=%v", err)
			}
			if err := w.WriteFile(context.Background(), "range.go", []byte("x")); !errors.Is(err, workspace.ErrNotRegular) {
				t.Fatalf("工作区链接应拒绝写入，err=%v", err)
			}

			// 用可信测试直接读根外样本，确认被拒绝的写入没有先沿链接修改它。
			after, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatalf("根外文件被修改，err=%v", err)
			}
		})
	}
}

// TestClosedWorkspace 验证关闭根句柄后，该 Workspace 实例的受控读写不再成功。
// 这里直接创建并显式 Close，避免把关闭时机藏在辅助函数的 Cleanup 中。
// 断言只要求返回错误，不指定错误类别，也没有额外验证结果目录是否保留。
func TestClosedWorkspace(t *testing.T) {
	// 正常创建是关闭契约的前提；源和父目录仍由测试框架负责最终删除。
	source, parent := makeSource(t), t.TempDir()
	w, err := workspace.Prepare(context.Background(), source, parent)
	if err != nil {
		t.Fatal(err)
	}

	// Close 释放目录句柄而非删除目录；显式关闭后再用正常 context 请求有效名称。
	// 这样操作失败不能由预取消或非法文件名来冒充关闭后的拒绝行为。
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 分别检查读和写不再成功，覆盖两个受控入口的关闭后行为。
	if _, err := w.ReadFile(context.Background(), "range.go"); err == nil {
		t.Fatal("关闭后不应读取")
	}
	if err := w.WriteFile(context.Background(), "range.go", []byte("x")); err == nil {
		t.Fatal("关闭后不应写入")
	}
}
