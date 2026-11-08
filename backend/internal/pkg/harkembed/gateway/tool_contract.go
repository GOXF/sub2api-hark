package gateway

import "net/http"

func requiresClientTool(v chatRequest) bool {
	return v.ToolChoiceNamed || v.ToolChoice != "" && v.ToolChoice != "auto" && v.ToolChoice != "none"
}

func validateClientToolRequest(v chatRequest, forwardExtra bool) error {
	if !requiresClientTool(v) {
		return nil
	}
	if forwardExtra {
		return &upstreamError{http.StatusBadRequest, "tool_choice_unsupported", "forward-extra 模式未启用客户端工具解析，请关闭该模式后使用必需工具调用"}
	}
	if len(v.Tools) == 0 {
		return &upstreamError{http.StatusBadRequest, "invalid_tool_choice", "必需工具调用要求客户端声明可用工具"}
	}
	if v.ToolChoice != "required" || v.ToolChoiceNamed {
		for _, tool := range v.Tools {
			if tool.Name == v.ToolChoice {
				return nil
			}
		}
		return &upstreamError{http.StatusBadRequest, "invalid_tool_choice", "指定工具不在客户端声明的工具列表中"}
	}
	return nil
}

func validateClientToolResult(v chatRequest, calls []toolCall) error {
	if !requiresClientTool(v) {
		return nil
	}
	// This turn has already been accepted upstream. A non-retryable 422 avoids
	// replaying it on another account; a textual promise is not an execution.
	if len(calls) == 0 {
		return &upstreamError{http.StatusUnprocessableEntity, "tool_call_missing", "上游未返回要求的客户端工具调用；本机执行尚未开始，请检查工具调用记录后显式继续"}
	}
	if v.ToolChoice != "required" || v.ToolChoiceNamed {
		for _, call := range calls {
			name := call.Name
			if call.Namespace != "" {
				name = call.Namespace + "." + name
			}
			if name != v.ToolChoice {
				return &upstreamError{http.StatusUnprocessableEntity, "tool_call_mismatch", "上游返回的客户端工具与指定工具不一致；本机执行尚未开始，请检查工具调用记录后显式继续"}
			}
		}
	}
	return nil
}
