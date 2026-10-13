package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

type ProjectInput struct {
	Project string `json:"project" jsonschema:"required,description=项目名称"`
}

type ProjectInfo struct {
	Language string `json:"language"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		message := err.Error()
		if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
			message = strings.ReplaceAll(message, key, "[REDACTED]")
		}
		if len(message) > 2048 {
			message = message[:2048] + "...（截断）"
		}
		slog.Error("工具调用验收失败", "err", message)
		os.Exit(1)
	}
}

func run() error {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return errors.New("缺少环境变量 DEEPSEEK_API_KEY")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	maxTokens := 512
	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:    apiKey,
		BaseURL:   "https://api.deepseek.com",
		Model:     "deepseek-flash",
		MaxCompletionTokens: &maxTokens,

		ExtraFields: map[string]any{
			"thinking": map[string]any{"type": "disabled"},
		},
	})
	if err != nil {
		return fmt.Errorf("创建模型失败: %w", err)
	}

	toolRuns := 0
	projectTool, err := utils.InferTool(
		"get_project_info",
		"查询项目的编程语言。目前只允许查询 CodePilot-Go。",
		func(ctx context.Context, input ProjectInput) (ProjectInfo, error) {
			if err := ctx.Err(); err != nil {
				return ProjectInfo{}, err
			}
			if input.Project != "CodePilot-Go" {
				return ProjectInfo{}, errors.New("只允许查询 CodePilot-Go")
			}
			if toolRuns >= 1 {
				return ProjectInfo{}, errors.New("工具执行预算已耗尽")
			}

			toolRuns++
			return ProjectInfo{Language: "Go"}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("创建工具失败: %w", err)
	}

	info, err := projectTool.Info(ctx)
	if err != nil {
		return fmt.Errorf("读取工具定义失败: %w", err)
	}

	toolModel, err := cm.WithTools([]*schema.ToolInfo{info})
	if err != nil {
		return fmt.Errorf("绑定工具失败: %w", err)
	}

	messages := []*schema.Message{
		schema.SystemMessage(
			"你必须先调用一次 get_project_info 查询项目。" +
				"收到工具结果后，不再调用工具，只回答语言名称，例如 Go。",
		),
		schema.UserMessage("查询 CodePilot-Go 使用的编程语言。"),
	}

	first, err := toolModel.Generate(ctx, messages)
	if err != nil {
		return fmt.Errorf("第一次模型调用失败: %w", err)
	}
	if first == nil || len(first.ToolCalls) != 1 {
		return errors.New("第一次响应必须包含且仅包含一个工具调用")
	}

	call := first.ToolCalls[0]
	if call.Function.Name != info.Name || call.ID == "" {
		return errors.New("工具名称不在允许范围内，或缺少调用 ID")
	}
	if len(call.Function.Arguments) > 1024 {
		return errors.New("工具参数超过 1024 字节限制")
	}

	result, err := projectTool.InvokableRun(ctx, call.Function.Arguments)
	if err != nil {
		return fmt.Errorf("执行工具失败: %w", err)
	}
	if len(result) > 512 {
		return errors.New("工具结果超过 512 字节限制")
	}

	slog.Info("受控工具已执行",
		"name", call.Function.Name,
		"result", result,
	)

	messages = append(messages,
		first,
		schema.ToolMessage(result, call.ID,
			schema.WithToolName(call.Function.Name)),
	)

	final, err := toolModel.Generate(ctx, messages)
	if err != nil {
		return fmt.Errorf("第二次模型调用失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if final == nil || len(final.ToolCalls) != 0 {
		return errors.New("第二次响应必须为最终回答")
	}
	if toolRuns != 1 || strings.TrimSpace(final.Content) != "Go" {
		return errors.New("实际工具执行次数或最终答案不符合验收要求")
	}

	fmt.Printf("PASS model_calls=2 tool_calls=%d answer=Go\n", toolRuns)
	return nil
}
