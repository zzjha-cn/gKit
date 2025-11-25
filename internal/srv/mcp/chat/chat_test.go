package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ThinkInAIXYZ/go-mcp/client"
	"github.com/ThinkInAIXYZ/go-mcp/protocol"
	"github.com/ThinkInAIXYZ/go-mcp/transport"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// ConversationState 对话状态枚举
type ConversationState int

const (
	StateWaitingForUser ConversationState = iota
	StateProcessingTools
	StateWaitingForConfirmation
	StateCompleted
	StateError
)

// ConversationTurn 单轮对话记录
type ConversationTurn struct {
	ID          string                                 `json:"id"`
	UserInput   string                                 `json:"user_input,omitempty"`
	AIResponse  string                                 `json:"ai_response,omitempty"`
	ToolCalls   []openai.ChatCompletionMessageToolCall `json:"tool_calls,omitempty"`
	ToolResults []ToolCallResult                       `json:"tool_results,omitempty"`
	State       ConversationState                      `json:"state"`
}

// ToolCallResult 工具调用结果
type ToolCallResult struct {
	ToolCallID string      `json:"tool_call_id"`
	ToolName   string      `json:"tool_name"`
	Result     interface{} `json:"result"`
	Error      string      `json:"error,omitempty"`
}

// ChatAgent 聊天代理结构
type ChatAgent struct {
	mcpClient      *client.Client
	openaiClient   openai.Client
	availableTools []protocol.Tool
	ctx            context.Context

	// 多轮对话支持
	conversationHistory []ConversationTurn
	currentState        ConversationState
	maxHistoryTurns     int
}

// NewChatAgent 创建新的聊天代理
func NewChatAgent(mcpServerURL, openaiAPIKey string) (*ChatAgent, error) {
	// 创建SSE传输客户端
	transportClient, err := transport.NewSSEClientTransport(mcpServerURL + "/mcp/sse")
	if err != nil {
		return nil, fmt.Errorf("创建传输客户端失败: %v", err)
	}

	// 创建MCP客户端
	mcpClient, err := client.NewClient(transportClient)
	if err != nil {
		return nil, fmt.Errorf("创建MCP客户端失败: %v", err)
	}

	// 创建OpenAI客户端
	openaiClient := openai.NewClient(
		// option.WithBaseURL("https://one-api.rd.cpp32.com/v1"),
		option.WithBaseURL("https://one-api-gb.rd.cpp32.com/v1"),
		option.WithAPIKey(openaiAPIKey),
	)

	agent := &ChatAgent{
		mcpClient:           mcpClient,
		openaiClient:        openaiClient,
		availableTools:      []protocol.Tool{},
		ctx:                 context.Background(),
		conversationHistory: make([]ConversationTurn, 0),
		currentState:        StateWaitingForUser,
		maxHistoryTurns:     20,
	}

	// 获取可用工具列表
	if err := agent.loadAvailableTools(); err != nil {
		return nil, fmt.Errorf("加载可用工具失败: %v", err)
	}

	return agent, nil
}

// Close 关闭客户端连接
func (ca *ChatAgent) Close() {
	if ca.mcpClient != nil {
		ca.mcpClient.Close()
	}
}

// loadAvailableTools 加载可用的MCP工具
func (ca *ChatAgent) loadAvailableTools() error {
	tools, err := ca.mcpClient.ListTools(ca.ctx)
	if err != nil {
		return fmt.Errorf("获取工具列表失败: %v", err)
	}

	// 转换工具列表
	ca.availableTools = make([]protocol.Tool, len(tools.Tools))
	for i, tool := range tools.Tools {
		ca.availableTools[i] = *tool
	}

	return nil
}

// listAvailableTools 列出可用的工具
func (ca *ChatAgent) listAvailableTools() error {
	fmt.Println("\n🔧 可用的MCP工具:")
	fmt.Println("==================")
	for _, tool := range ca.availableTools {
		fmt.Printf("  📋 %s: %s\n", tool.Name, tool.Description)
	}
	fmt.Println("==================\n")
	return nil
}

// callMCPTool 调用MCP工具
func (ca *ChatAgent) callMCPTool(toolName string, arguments map[string]interface{}) (*protocol.CallToolResult, error) {
	request := &protocol.CallToolRequest{
		Name:      toolName,
		Arguments: arguments,
	}

	result, err := ca.mcpClient.CallTool(ca.ctx, request)
	if err != nil {
		return nil, fmt.Errorf("调用工具失败: %v", err)
	}

	return result, nil
}

// buildToolsForLLM 为LLM构建工具描述
func (ca *ChatAgent) buildToolsForLLM() []openai.ChatCompletionToolParam {
	var tools []openai.ChatCompletionToolParam

	for _, tool := range ca.availableTools {
		// 动态从MCP工具的InputSchema获取参数schema
		var properties map[string]interface{}
		var required []string

		// 检查InputSchema是否存在并转换Properties
		if tool.InputSchema.Properties != nil {
			properties = convertPropertiesToMap(tool.InputSchema.Properties)
		} else {
			// 如果没有properties，提供一个空的对象schema以符合OpenAI API要求
			properties = map[string]interface{}{}
		}

		// 获取required字段
		if tool.InputSchema.Required != nil {
			required = tool.InputSchema.Required
		} else {
			required = []string{}
		}

		llmTool := openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters: openai.FunctionParameters{
					"type":       "object",
					"properties": properties,
					"required":   required,
				},
			},
		}

		tools = append(tools, llmTool)
	}

	return tools
}

// convertPropertiesToMap 将protocol.Property映射转换为map[string]interface{}
func convertPropertiesToMap(properties map[string]*protocol.Property) map[string]interface{} {
	result := make(map[string]interface{})

	for key, prop := range properties {
		if prop != nil {
			result[key] = convertPropertyToMap(prop)
		}
	}

	return result
}

// convertPropertyToMap 将单个Property转换为完整的JSON Schema map
func convertPropertyToMap(prop *protocol.Property) map[string]interface{} {
	propMap := map[string]interface{}{
		"type": string(prop.Type),
	}

	// 添加描述
	if prop.Description != "" {
		propMap["description"] = prop.Description
	}

	// 处理对象类型的嵌套属性
	if prop.Type == "object" && prop.Properties != nil {
		propMap["properties"] = convertPropertiesToMap(prop.Properties)

		// 添加必需字段
		if len(prop.Required) > 0 {
			propMap["required"] = prop.Required
		}
	}

	// 处理数组类型的items
	if prop.Type == "array" && prop.Items != nil {
		propMap["items"] = convertPropertyToMap(prop.Items)
	}

	// 处理枚举值
	if len(prop.Enum) > 0 {
		propMap["enum"] = prop.Enum
	}

	// 处理默认值
	if prop.Default != nil {
		propMap["default"] = prop.Default
	}

	return propMap
}

// processWithLLMMultiTurn 多轮对话处理
func (ca *ChatAgent) processWithLLMMultiTurn(userInput string) error {
	// 创建新的对话轮次
	turnID := fmt.Sprintf("turn_%d", len(ca.conversationHistory)+1)
	currentTurn := ConversationTurn{
		ID:        turnID,
		UserInput: userInput,
		State:     StateProcessingTools,
	}

	// 构建包含历史的消息列表
	messages := ca.buildMessagesWithHistory(userInput)
	tools := ca.buildToolsForLLM()

	// 开始多轮处理循环
	maxIterations := 5 // 防止无限循环
	for iteration := 0; iteration < maxIterations; iteration++ {
		fmt.Printf("🔄 处理轮次 %d...\n", iteration+1)

		// 调用OpenAI API
		completion, err := ca.openaiClient.Chat.Completions.New(ca.ctx, openai.ChatCompletionNewParams{
			// Model:    "qwen-plus",
			Model:    "gpt-4o",
			Messages: messages,
			Tools:    tools,
		})

		if err != nil {
			return fmt.Errorf("调用OpenAI API失败: %v", err)
		}

		choice := completion.Choices[0]
		currentTurn.AIResponse = choice.Message.Content

		// 检查是否需要调用工具
		if len(choice.Message.ToolCalls) > 0 {
			fmt.Printf("🔧 需要调用 %d 个工具\n", len(choice.Message.ToolCalls))

			// 显示将要调用的工具
			for i, toolCall := range choice.Message.ToolCalls {
				fmt.Printf("   %d. %s\n", i+1, toolCall.Function.Name)
				fmt.Printf("   参数: %s\n", toolCall.Function.Arguments)
			}

			// 询问用户是否继续
			if !ca.confirmToolExecution(choice.Message.ToolCalls) {
				fmt.Println("❌ 用户取消了工具执行")
				break
			}

			// 执行工具调用
			toolResults, err := ca.executeToolCallsWithResults(choice.Message.ToolCalls)
			if err != nil {
				fmt.Printf("❌ 工具执行失败: %v\n", err)
				break
			}

			currentTurn.ToolCalls = choice.Message.ToolCalls
			currentTurn.ToolResults = toolResults

			// 正确的工具调用消息格式 - 基于原始processWithLLM的实现
			// 1. 首先添加包含工具调用的assistant消息
			messages = append(messages, openai.AssistantMessage(choice.Message.Content))

			// 2. 为每个工具调用添加对应的tool消息
			for _, result := range toolResults {
				resultContent := ""
				if result.Error != "" {
					resultContent = fmt.Sprintf("错误: %s", result.Error)
				} else {
					resultBytes, _ := json.Marshal(result.Result)
					resultContent = string(resultBytes)
				}

				messages = append(messages, openai.ToolMessage(result.ToolCallID, resultContent))
			}

			// 检查是否需要继续处理
			if ca.shouldContinueProcessing(toolResults) {
				fmt.Println("🔄 基于工具结果继续处理...")
				continue
			} else {
				fmt.Println("✅ 工具执行完成")
				break
			}
		} else {
			// 没有工具调用，直接显示回答
			fmt.Printf("🤖 AI助手: %s\n", choice.Message.Content)
			break
		}
	}

	// 工具调用完成后，生成最终的综合响应
	if len(currentTurn.ToolResults) > 0 {
		fmt.Println("📝 正在生成最终响应...")

		// 简化的方式：直接构建一个包含工具结果的用户消息
		toolResultsSummary := "以下是工具调用的结果：\n\n"

		for i, result := range currentTurn.ToolResults {
			toolResultsSummary += fmt.Sprintf("工具 %d (%s):\n", i+1, result.ToolName)
			if result.Error != "" {
				toolResultsSummary += fmt.Sprintf("错误: %s\n", result.Error)
			} else {
				resultBytes, _ := json.Marshal(result.Result)
				toolResultsSummary += fmt.Sprintf("结果: %s\n", string(resultBytes))
			}
			toolResultsSummary += "\n"
		}

		toolResultsSummary += "请基于以上工具调用的结果，为用户生成一个清晰、完整的回答，将数据整理为markdown表格。"

		fmt.Println("最终Prompt：")
		fmt.Println(toolResultsSummary)
		fmt.Println("======================")

		// 构建包含工具结果摘要的消息
		finalMessages := ca.buildMessagesWithHistory(userInput)
		finalMessages = append(finalMessages, openai.UserMessage(toolResultsSummary))

		// 调用大模型生成最终响应
		finalCompletion, err := ca.openaiClient.Chat.Completions.New(ca.ctx, openai.ChatCompletionNewParams{
			Model:    "gpt-4o",
			Messages: finalMessages,
		})

		if err != nil {
			fmt.Printf("❌ 生成最终响应失败: %v\n", err)
		} else {
			finalResponse := finalCompletion.Choices[0].Message.Content
			currentTurn.AIResponse = finalResponse
			fmt.Printf("🤖 AI助手最终回答: %s\n", finalResponse)
		}
	}

	// 保存对话轮次
	currentTurn.State = StateCompleted
	ca.addToHistory(currentTurn)

	return nil
}

// buildMessagesWithHistory 构建包含历史对话的消息列表
func (ca *ChatAgent) buildMessagesWithHistory(currentInput string) []openai.ChatCompletionMessageParamUnion {
	systemPrompt := `你是一个智能助手，可以帮助用户完成各种任务。你有以下工具可以使用：

1. calculator: 计算器工具，可以进行加减乘除运算
2. agg_line: 数据聚合工具，可以按维度对数据进行聚合

在多轮对话中，你需要：
1. 理解用户的完整意图，可能需要多次工具调用
2. 合理规划工具调用顺序
3. 基于之前的结果做出后续决策
4. 在必要时询问用户更多信息
5. 提供清晰的执行反馈

请根据用户需求和对话历史，决定是否需要调用工具。如果需要多步骤处理，请逐步执行。`

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
	}

	// 添加历史对话（保持在合理范围内）
	historyStart := 0
	if len(ca.conversationHistory) > ca.maxHistoryTurns {
		historyStart = len(ca.conversationHistory) - ca.maxHistoryTurns
	}

	for i := historyStart; i < len(ca.conversationHistory); i++ {
		turn := ca.conversationHistory[i]

		// 添加用户输入
		if turn.UserInput != "" {
			messages = append(messages, openai.UserMessage(turn.UserInput))
		}

		// 如果有工具调用，需要按正确顺序添加消息
		if len(turn.ToolCalls) > 0 {
			// 1. 添加包含工具调用的assistant消息
			messages = append(messages, openai.AssistantMessage(turn.AIResponse))

			// 2. 为每个工具调用添加对应的tool消息
			for _, result := range turn.ToolResults {
				resultContent := ""
				if result.Error != "" {
					resultContent = fmt.Sprintf("错误: %s", result.Error)
				} else {
					resultBytes, _ := json.Marshal(result.Result)
					resultContent = string(resultBytes)
				}
				messages = append(messages, openai.ToolMessage(result.ToolCallID, resultContent))
			}
		} else if turn.AIResponse != "" {
			// 没有工具调用的普通AI响应
			messages = append(messages, openai.AssistantMessage(turn.AIResponse))
		}
	}

	// 添加当前用户输入
	messages = append(messages, openai.UserMessage(currentInput))

	return messages
}

// confirmToolExecution 确认工具执行
func (ca *ChatAgent) confirmToolExecution(toolCalls []openai.ChatCompletionMessageToolCall) bool {
	fmt.Print("❓ 是否继续执行这些工具？(y/n): ")
	var response string
	fmt.Scanln(&response)
	return strings.ToLower(response) == "y" || strings.ToLower(response) == "yes"
}

// executeToolCallsWithResults 执行工具调用并返回结果
func (ca *ChatAgent) executeToolCallsWithResults(toolCalls []openai.ChatCompletionMessageToolCall) ([]ToolCallResult, error) {
	results := make([]ToolCallResult, 0, len(toolCalls))

	for _, toolCall := range toolCalls {
		result := ToolCallResult{
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Function.Name,
		}

		// 解析工具参数
		var arguments map[string]interface{}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &arguments); err != nil {
			result.Error = fmt.Sprintf("参数解析失败: %v", err)
		} else {
			// 调用MCP工具
			mcpResult, err := ca.callMCPTool(toolCall.Function.Name, arguments)
			if err != nil {
				result.Error = fmt.Sprintf("工具调用失败: %v", err)
			} else {
				result.Result = mcpResult
				// 显示结果
				ca.displayResponse(toolCall.Function.Name, mcpResult)
			}
		}

		results = append(results, result)
	}

	return results, nil
}

// shouldContinueProcessing 判断是否需要继续处理
func (ca *ChatAgent) shouldContinueProcessing(results []ToolCallResult) bool {
	// 检查是否有工具调用失败，可能需要重试或采用其他方案
	for _, result := range results {
		if result.Error != "" {
			fmt.Printf("⚠️  工具 %s 执行失败: %s\n", result.ToolName, result.Error)
			return true // 可能需要重试或使用其他工具
		}
	}

	// 可以根据具体业务逻辑决定是否继续
	// 这里简化处理，通常工具执行成功后就结束
	return false
}

// addToHistory 添加到对话历史
func (ca *ChatAgent) addToHistory(turn ConversationTurn) {
	ca.conversationHistory = append(ca.conversationHistory, turn)

	// 保持历史在合理范围内
	if len(ca.conversationHistory) > ca.maxHistoryTurns*2 {
		// 保留最近的对话
		ca.conversationHistory = ca.conversationHistory[ca.maxHistoryTurns:]
	}
}

// showConversationHistory 显示对话历史
func (ca *ChatAgent) showConversationHistory() {
	fmt.Printf("\n📚 对话历史 (共 %d 轮):\n", len(ca.conversationHistory))
	fmt.Println("==========================================")

	for i, turn := range ca.conversationHistory {
		fmt.Printf("\n--- 第 %d 轮 (ID: %s) ---\n", i+1, turn.ID)
		if turn.UserInput != "" {
			fmt.Printf("👤 用户: %s\n", turn.UserInput)
		}
		if turn.AIResponse != "" {
			fmt.Printf("🤖 助手: %s\n", turn.AIResponse)
		}
		if len(turn.ToolCalls) > 0 {
			fmt.Printf("🔧 工具调用: %d 个\n", len(turn.ToolCalls))
			for j, toolCall := range turn.ToolCalls {
				fmt.Printf("   %d. %s\n", j+1, toolCall.Function.Name)
			}
		}
		if len(turn.ToolResults) > 0 {
			fmt.Printf("📊 工具结果: %d 个\n", len(turn.ToolResults))
		}
	}
	fmt.Println("==========================================")
}

// clearConversationHistory 清空对话历史
func (ca *ChatAgent) clearConversationHistory() {
	ca.conversationHistory = make([]ConversationTurn, 0)
	fmt.Println("🗑️  对话历史已清空")
}

// displayResponse 显示MCP响应
func (ca *ChatAgent) displayResponse(toolName string, result *protocol.CallToolResult) {
	if result == nil {
		fmt.Printf("⚠️ %s返回了空结果\n", toolName)
		return
	}

	fmt.Printf("✅ %s结果:\n", toolName)

	for i, content := range result.Content {
		switch c := content.(type) {
		case *protocol.TextContent:
			if len(result.Content) > 1 {
				fmt.Printf("📄 内容 %d:\n%s\n", i+1, c.Text)
			} else {
				fmt.Printf("%s\n", c.Text)
			}
		case *protocol.ImageContent:
			fmt.Printf("🖼️ 图片内容 %d: %s\n", i+1, c.Data)
		default:
			fmt.Printf("📋 其他内容 %d: %+v\n", i+1, content)
		}
	}

	if result.IsError {
		fmt.Printf("⚠️ 工具执行过程中可能存在问题\n")
	}
}

// processUserInput 处理用户输入
func (ca *ChatAgent) processUserInput(input string) {
	input = strings.TrimSpace(input)

	if input == "" {
		return
	}

	// 退出命令
	if input == "退出" || input == "exit" || input == "quit" {
		fmt.Println("再见！")
		ca.Close()
		os.Exit(0)
	}

	// 列出工具命令
	if input == "工具" || input == "tools" || input == "list" {
		if err := ca.listAvailableTools(); err != nil {
			fmt.Printf("❌ 获取工具列表失败: %v\n", err)
		}
		return
	}

	// 显示对话历史
	if input == "历史" || input == "history" {
		ca.showConversationHistory()
		return
	}

	// 清空对话历史
	if input == "清空" || input == "clear" {
		ca.clearConversationHistory()
		return
	}

	// 使用多轮对话处理用户输入
	if err := ca.processWithLLMMultiTurn(input); err != nil {
		fmt.Printf("❌ 处理请求失败: %v\n", err)

		// 如果LLM处理失败，尝试使用传统方式处理简单命令
		// ca.fallbackProcessing(input)
	}
}

// startChat 启动聊天界面
func (ca *ChatAgent) startChat() {
	fmt.Println("🤖 AI智能助手已启动! (LLM + MCP工具集成)")
	fmt.Println("输入 '帮助' 查看功能介绍，输入 '退出' 结束对话")

	// 尝试列出可用工具
	fmt.Println("正在连接MCP服务器并获取可用工具...")
	if err := ca.listAvailableTools(); err != nil {
		fmt.Printf("⚠️ 无法获取工具列表: %v\n", err)
		fmt.Println("请确保MCP服务器正在运行")
	}

	fmt.Println("====================")
	fmt.Println("💡 现在您可以用自然语言与我对话，我会智能地为您调用合适的工具！")
	fmt.Println("🔄 支持多轮对话和复杂任务的多步骤处理")

	// scanner := bufio.NewScanner(os.Stdin)

	// for {
	// 	fmt.Print("\n💬 您: ")
	// 	if !scanner.Scan() {
	// 		break
	// 	}

	// 	input := scanner.Text()
	// 	ca.processUserInput(input)
	// }

	// if err := scanner.Err(); err != nil {
	// 	fmt.Printf("读取输入时发生错误: %v\n", err)
	// }
	// ca.processUserInput("帮我计算15 * 232332")
	ca.processUserInput(`
帮我将这些创意按照命名的前缀分组聚合出总和。
步骤：
- 先根据命名按照提供的类别对数据分组
- 将每个类别分组不断调用数据合并工具，计算总和

数据：
| 创意 | 转化数 | CPA |
| ---| --- | --- |
| native-display_Prada_WW_T1_1280X720 | 5.000 | 206.341 |
| native-display_MW_T4_1280X720 | 4.000 | 311.588 |
| native-display_KOL HiyaSonya_v1_T1_1280X720 | 3.000 | 131.035 |
| native-display_Prada_WW_T3_1280X720 | 3.000 | 293.019 |
| native-display_The Row_WW_T5_1280X720 | 3.000 | 373.044 |
`)
}

func main() {
	// MCP服务器地址
	mcpServerURL := "http://localhost:8080"

	// OpenAI API Key
	openaiAPIKey := os.Getenv("OPENAI_API_KEY")
	if openaiAPIKey == "" {
		log.Fatal("请设置OPENAI_API_KEY环境变量")
	}

	// 检查命令行参数
	if len(os.Args) > 1 {
		mcpServerURL = os.Args[1]
	}

	// 创建聊天代理
	agent, err := NewChatAgent(mcpServerURL, openaiAPIKey)
	if err != nil {
		log.Fatalf("创建聊天代理失败: %v", err)
	}
	defer agent.Close()

	// 启动聊天界面
	agent.startChat()
}
