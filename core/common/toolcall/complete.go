package toolcall

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"chatgpt-adapter/core/cache"
	"chatgpt-adapter/core/common"
	"chatgpt-adapter/core/common/agent"
	"chatgpt-adapter/core/common/vars"
	"chatgpt-adapter/core/gin/model"
	"chatgpt-adapter/core/gin/response"
	"chatgpt-adapter/core/logger"
	"github.com/dlclark/regexp2"
	"github.com/gin-gonic/gin"
)

var (
	exclude_tool_names    = "__exclude-tool-names__"
	exclude_task_contents = "__exclude-task-contents__"
	MaxMessages           = 20
)

func NeedExec(ctx *gin.Context) bool {
	var tool = "-1"
	{
		t := common.GetGinToolValue(ctx)
		if !t.Is("enabled", true) {
			return false
		}

		tool = t.GetString("id")
		if tool == "-1" && t.Is("tasks", true) {
			tool = "tasks"
		}
	}

	completion := common.GetGinCompletion(ctx)
	messageL := len(completion.Messages)
	if messageL == 0 {
		return false
	}

	if len(completion.Tools) == 0 {
		return false
	}

	role := completion.Messages[messageL-1]["role"]
	return (role != "function" && role != "tool") || tool != "-1"
}

func Cancel(str string) bool {
	str = strings.TrimSpace(str)
	if strings.Contains(str, "<|tool|>") {
		return true
	}
	if strings.Contains(str, "<|assistant|>") {
		return true
	}
	if strings.Contains(str, "<|user|>") {
		return true
	}
	if strings.Contains(str, "<|system|>") {
		return true
	}
	if strings.Contains(str, "<|tool_response|>") {
		return true
	}
	if strings.Contains(str, "<|end|>") {
		return true
	}
	if strings.Contains(str, "USER: ") {
		return true
	}
	if strings.Contains(str, "ANSWER: ") {
		return true
	}
	if strings.Contains(str, "TOOL_RESPONSE: ") {
		return true
	}
	// return len(str) > 1 && !strings.HasPrefix(str, "1:")
	return false
}

// 工具名是否存在工具集中，"-1" 不存在，否则返回具体名字
func Query(key string, tools []model.Keyv[interface{}]) (value string) {
	value = key
	if value == "" || value == "-1" {
		return "-1"
	}

	if len(tools) == 0 {
		return "-1"
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		if fn.Has("name") {
			if value == fn.GetString("name") {
				return
			}
		}

		if fn.Has("id") && value == fn.GetString("id") {
			value = fn.GetString("name")
			return
		}
	}

	return "-1"
}

// 执行工具选择器
//
//	return:
//	bool  > 是否执行了工具
//	error > 执行异常
func ToolChoice(ctx *gin.Context, completion model.Completion, callback func(message string) (string, error)) (bool, error) {
	cacheManager := cache.ToolTasksCacheManager()
	ctx.Set(exclude_task_contents, "")
	defer logger.Info("completeToolCalls called")

	// 是否开启任务拆解
	if tasksIsEnabled(ctx) {
		var hasTasks = false
		toolCache := hex(completion)
		if completion.Messages, hasTasks = taskComplete(ctx, completion, callback); !hasTasks {
			// 非-1值则为有默认选项
			valueDef := Query(common.GetGinToolValue(ctx).GetString("id"), completion.Tools)
			if valueDef != "-1" {
				return toolCallResponse(ctx, completion, valueDef, "{}", time.Now().Unix()), nil
			}
			return false, nil
		}

		// 无参数task跳过提示词收集
		tasks, err := cacheManager.GetValue(toolCache)
		if err != nil {
			logger.Error(err)
		}

		for _, task := range tasks {
			if !task.Is("exclude", "true") {
				name, q := nameWithToolsNotArgs(task, completion.Tools)
				if name != "-1" {
					value := "{}"
					if q != "" { // 提供特殊字段
						value = q
						logger.Infof("$query: %s", value)
					}
					return toolCallResponse(ctx, completion, name, value, time.Now().Unix()), nil
				}
				// 只判断一次
				break
			}
		}
	}

	// toolChoice自推荐toolId处理
	if completion.ToolChoice != "" && completion.ToolChoice != "auto" {
		var (
			keyv       model.Keyv[interface{}]
			toolChoice model.Keyv[interface{}]
			ok         = false
		)
		toolChoice, ok = completion.ToolChoice.(map[string]interface{})
		if !ok || !toolChoice.Is("type", "function") {
			goto label
		}

		keyv = toolChoice.GetKeyv("function")
		if !keyv.Has("name") {
			goto label
		}

		if toolId := toolIdWithTools(keyv.GetString("name"), completion.Tools); toolId != "-1" {
			completion.Messages = append(completion.Messages, model.Keyv[interface{}]{
				"role": "user", "content": "continue。 工具推荐： toolId = " + toolId,
			})
		}
	label:
	}

	message, err := buildTemplate(ctx, completion, agent.ToolCall)
	if err != nil {
		return false, err
	}

	content, err := callback(message)
	if err != nil {
		return false, err
	}

	previousTokens := response.CalcTokens(message)
	ctx.Set(vars.GinCompletionUsage, response.CalcUsageTokens(content, previousTokens))

	// 解析参数
	return parseToTC(ctx, content, completion), nil
}

// 拆解任务, 组装任务提示并返回上下文 (包含缓存已执行的任务逻辑)
func taskComplete(ctx *gin.Context, completion model.Completion, callback func(message string) (string, error)) (messages []model.Keyv[interface{}], hasTasks bool) {
	cacheManager := cache.ToolTasksCacheManager()
	messages = completion.Messages
	message, err := buildTemplate(ctx, completion, agent.ToolTasks)
	if err != nil {
		logger.Error(err)
		return
	}

	toolCache := hex(completion)
	logger.Infof("completeTasks calc hash - %s", toolCache)
	tasks, err := cacheManager.GetValue(toolCache)
	if err != nil {
		logger.Error(err)
	}

	if tasks != nil {
		excludeTasks(completion, tasks)
		logger.Infof("completeTasks response: <cached> %s", tasks)
		// 刷新缓存时间
		if err = cacheManager.SetValue(toolCache, tasks); err != nil {
			logger.Error(err)
		}
	} else {
		content, e := callback(message)
		if e != nil {
			logger.Error(e)
			return
		}
		logger.Infof("completeTasks response: \n%s", content)

		// 解析参数
		tasks = parseToTT(content, completion)
		if len(tasks) == 0 {
			return
		}

		excludeTasks(completion, tasks)
		// 刷新缓存时间
		if err = cacheManager.SetValue(toolCache, tasks); err != nil {
			logger.Error(err)
		}
	}

	// 任务提示组装
	var excTasks []string
	var contents []string
	for pos := range tasks {
		task := tasks[pos]
		toolId := task.GetString("toolId")
		if task.Is("exclude", "true") {
			excTasks = append(excTasks, fmt.Sprintf("工具[%s]%s已执行", toolIdWithTools(toolId, completion.Tools), task.GetString("task")))
		} else {
			contents = append(contents, task.GetString("task")+"。 工具推荐： toolId = "+toolIdWithTools(toolId, completion.Tools))
		}
	}

	if len(contents) == 0 {
		return messages, false
	}

	hasTasks = true
	logger.Infof("completeTasks excludeTasks: %s", excTasks)
	logger.Infof("completeTasks nextTask: %s", contents[0])
	ctx.Set(exclude_task_contents, strings.Join(excTasks, "，"))

	// 拼接任务信息
	for pos := len(messages) - 1; pos > 0; pos-- {
		if messages[pos].Is("role", "user") {
			messages = append(messages[:pos], messages[pos+1:]...)
			break
		}
	}
	messages = append(messages, model.Keyv[interface{}]{
		"role": "user", "content": contents[0],
	})

	return
}

func hex(completion model.Completion) (hash string) {
	messages := completion.Messages
	messageL := len(messages)
	count := 3 // 只获取后3条
	for pos := messageL - 1; pos > 0; pos-- {
		message := messages[pos]
		if message.Is("role", "user") {
			hash += message.GetString("content")
			count--
			if count == 0 {
				break
			}
		}
	}

	if hash == "" {
		return "-1"
	}

	mod := completion.Model
	// 一些前缀匹配的的AI model，特殊处理
	if pos := strings.Index(mod, "/"); pos > -1 {
		switch mod[:pos] {
		case "coze":
			s := ""
			if strings.HasSuffix(mod, "-o") || strings.HasSuffix(mod, "-w") {
				s = mod[len(mod)-2:]
			}
			mod = "coze" + s

		case "custom", "lmsys":
			mod = mod[pos+1:]
		}
	}
	return common.CalcHex(mod + hash)
}

func buildTemplate(ctx *gin.Context, completion model.Completion, template string) (message string, err error) {
	pMessages := completion.Messages
	messageL := len(pMessages)
	content := "continue"

	if messageL > MaxMessages {
		pMessages = pMessages[messageL-MaxMessages:]
		messageL = len(pMessages)
	}

	if messageL > 0 {
		if keyv := pMessages[messageL-1]; keyv.Is("role", "user") {
			content = keyv.GetString("content")
			pMessages = pMessages[:messageL-1]
		}
	}

	slice := make([]model.Keyv[interface{}], messageL)
	for i, obj := range pMessages {
		c := obj.Clone()
		str := c.GetString("content")
		if str != "" {
			reg := regexp2.MustCompile(`<thinking_format>[\s\S]+</thinking_format>`, regexp2.Compiled)
			str, _ = reg.Replace(str, "", -1, -1)
			c.Set("content", str)
		}
		slice[i] = c
	}
	pMessages = slice

	if content == "continue" {
		ctx.Set(exclude_tool_names, extractToolNames(completion.Messages))
	}

	for _, t := range completion.Tools {
		fn := t.GetKeyv("function")
		// 避免重复使用时被替换
		if !fn.Has("id") {
			fn.Set("id", common.Hex(5))
		}
	}

	value, _ := ctx.Get(exclude_task_contents)
	str, err := newBuilder("tool").
		Vars("toolDef", getToolId(ctx, completion.Tools)).
		Vars("tools", completion.Tools).
		Vars("pMessages", pMessages).
		Vars("excludeTaskContents", value).
		Vars("content", content).
		Func("ToolId", func(str string) string {
			return toolIdWithTools(str, completion.Tools)
		}).
		Func("Join", func(slice []interface{}, sep string) string {
			if len(slice) == 0 {
				return ""
			}
			var result []string
			for _, v := range slice {
				result = append(result, fmt.Sprintf("\"%v\"", v))
			}
			return strings.Join(result, sep)
		}).
		Func("Has", func(obj map[string]interface{}, key string) bool {
			_, exists := obj[key]
			return exists
		}).
		Func("Len", func(slice []interface{}) int {
			return len(slice)
		}).
		Func("Enc", func(value interface{}) string {
			return strings.ReplaceAll(fmt.Sprintf("%s", value), "\n", "\\n")
		}).
		Func("ToolDesc", func(value string) string {
			for _, t := range completion.Tools {
				fn := t.GetKeyv("function")
				if !fn.Has("name") {
					continue
				}
				if value == fn.GetString("name") {
					return fn.GetString("description")
				}
			}
			return ""
		}).String(template)
	if err != nil {
		return
	}

	regMap := map[*regexp.Regexp]string{
		regexp.MustCompile(`<\|system\|>[\n|\s]+<\|end\|>`):           "",
		regexp.MustCompile(`<\|user\|>[\n|\s]+<\|end\|>`):             "",
		regexp.MustCompile(`<\|assistant\|>[\n|\s]+<\|end\|>`):        "",
		regexp.MustCompile(`<\|<no value>\|>\n<no value>\n<\|end\|>`): "",
		regexp.MustCompile(`\n{3}`):                                   "\n",
		regexp.MustCompile(`\n{2,}<\|end\|>`):                         "\n<|end|>",
	}
	for reg, v := range regMap {
		str = reg.ReplaceAllString(str, v)
	}

	message = strings.TrimSpace(str)
	return
}

// 工具参数解析
//
//	return:
//	bool  > 是否执行了工具
func parseToTC(ctx *gin.Context, content string, completion model.Completion) bool {
	j := ""
	created := time.Now().Unix()
	slice := strings.Split(content, "TOOL_RESPONSE")

	for _, value := range slice {
		left := strings.Index(value, "{")
		right := strings.LastIndex(value, "}")
		if left >= 0 && right > left {
			j = value[left : right+1]
			break
		}
	}

	// 非-1值则为有默认选项
	valueDef := Query(common.GetGinToolValue(ctx).GetString("id"), completion.Tools)

	// 没有解析出 JSON
	if j == "" {
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		logger.Infof("completeTools response failed: \n%s", content)
		return false
	}

	var fn model.Keyv[interface{}]
	name := ""
	for _, t := range completion.Tools {
		fn = t.GetKeyv("function")
		n := fn.GetString("name")
		// id 匹配
		if strings.Contains(j, fn.GetString("id")) {
			name = n
			break
		}
		// name 匹配
		if strings.Contains(j, n) {
			name = n
			break
		}
	}

	// 没有匹配到工具
	if name == "" {
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		logger.Infof("completeTools response failed: \n%s", content)
		return false
	}

	// 避免AI重复选择相同的工具
	if names, ok := common.GetGinValues[string](ctx, exclude_tool_names); ok {
		if slices.Contains(names, name) {
			return valueDef != "-1" && toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
	}

	// 解析参数
	var js model.Keyv[interface{}]
	if err := json.Unmarshal([]byte(j), &js); err != nil {
		logger.Error(err)
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		logger.Infof("completeTools response failed: \n%s", content)
		return false
	}

	logger.Infof("completeTools response: \n%s", j)
	obj, ok := js["arguments"]
	if !ok {
		// 尽可能解析，AI貌似十分喜欢将参数改为parameters
		if js.Has("parameters") &&
			!fn.GetKeyv("parameters").
				GetKeyv("properties").
				Has("parameters") {
			obj = js.GetKeyv("parameters")
		} else {
			delete(js, "toolId")
			obj = js
		}
	}

	bytes, _ := json.Marshal(obj)
	return toolCallResponse(ctx, completion, name, string(bytes), created)
}

// 解析任务
func parseToTT(content string, completion model.Completion) (tasks []model.Keyv[string]) {
	j := ""
	slice := strings.Split(content, "1: ")
	for _, value := range slice {
		left := strings.Index(value, "[")
		right := strings.LastIndex(value, "]")
		if left >= 0 && right > left {
			j = value[left : right+1]
			break
		}
	}

	// 没有解析出 JSON
	if j == "" {
		logger.Error("没有解析出 JSON")
		return
	}

	// 解析参数
	var js []model.Keyv[string]
	if err := json.Unmarshal([]byte(j), &js); err != nil {
		logger.Error(err)
		return
	}

	// 检查任务列表
	for pos := range js {
		task := js[pos]
		if !task.Has("toolId") {
			continue
		}

		toolId := Query(task.GetString("toolId"), completion.Tools)
		if toolId == "-1" || !task.Has("task") {
			continue
		}

		task.Set("toolId", toolId)
		tasks = append(tasks, task)
	}

	return
}

// 给已执行的工具打上标记
func excludeTasks(completion model.Completion, tasks []model.Keyv[string]) {
	if len(tasks) == 0 {
		return
	}

	excludeNames := extractToolNames(completion.Messages)
	for pos := range tasks {
		task := tasks[pos]

		toolId := Query(task.GetString("toolId"), completion.Tools)
		if toolId == "-1" || !task.Has("task") { // 不存在的任务过滤
			continue
		}

		// 标记是否已执行
		task.Set("exclude", strconv.FormatBool(slices.Contains(excludeNames, toolId)))
	}
}

// 提取对话中的tool-names
func extractToolNames(messages []model.Keyv[interface{}]) (slice []string) {
	index := len(messages) - MaxMessages
	if index < 0 {
		index = 0
	}

	for pos := range messages[index:] {
		message := messages[index+pos]
		if message.Is("role", "tool") {
			slice = append(slice, message.GetString("name"))
		}
	}
	return
}

func toolCallResponse(ctx *gin.Context, completion model.Completion, name string, value string, created int64) bool {
	if completion.Stream {
		response.SSEToolCallResponse(ctx, completion.Model, name, value, created)
		return true
	} else {
		response.ToolCallResponse(ctx, completion.Model, name, value)
		return true
	}
}

// 获取默认的toolId
func getToolId(ctx *gin.Context, tools []model.Keyv[interface{}]) (value string) {
	value = common.GetGinToolValue(ctx).GetString("id")
	if value == "-1" {
		return
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		toolId := fn.GetString("id")
		if fn.Has("name") {
			if value == fn.GetString("name") {
				value = toolId
				return
			}
		}
	}
	return "-1"
}

// 工具名是否存在工具集中，"-1" 不存在，否则返回具体toolId
func toolIdWithTools(name string, tools []model.Keyv[interface{}]) (value string) {
	value = name
	if value == "" {
		return "-1"
	}

	if len(tools) == 0 {
		return "-1"
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		if fn.Has("id") && value == fn.GetString("id") {
			return
		}

		if fn.Has("name") {
			if fn.Has("id") && value == fn.GetString("name") {
				value = fn.GetString("id")
				return
			}

			value = fn.GetString("name")
			if name == value {
				if fn.Has("id") {
					value = fn.GetString("id")
				}
				return
			}
		}
	}

	return "-1"
}

// 获取对应无参tools的name，没有则返回 -1
func nameWithToolsNotArgs(task model.Keyv[string], tools []model.Keyv[interface{}]) (value, q string) {
	value = task.GetString("toolId")
	if value == "" || value == "-1" {
		return "-1", ""
	}

	if len(tools) == 0 {
		return "-1", ""
	}

	hasK := func(keyv model.Keyv[interface{}]) bool {
		for k, v := range keyv {
			if vv, ok := v.(map[string]interface{}); ok && vv["description"].(string) == "$" { // 提供这个特殊值
				q = fmt.Sprintf(`{"%s":%s}`, k, strconv.Quote(task.GetString("task")))
				continue
			}
			return true
		}
		return false
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		if fn.Has("name") {
			if value == fn.GetString("name") {
				keyv := fn.GetKeyv("parameters")
				if keyv.Has("properties") && hasK(keyv.GetKeyv("properties")) {
					continue
				}
				return
			}
		}

		if fn.Has("id") && value == fn.GetString("id") {
			value = fn.GetString("name")
			keyv := fn.GetKeyv("parameters")
			if keyv.Has("properties") && hasK(keyv.GetKeyv("properties")) {
				continue
			}
			return
		}
	}

	return "-1", ""
}

func tasksIsEnabled(ctx *gin.Context) bool {
	completion := common.GetGinCompletion(ctx)
	if completion.ToolChoice != "" && completion.ToolChoice != "auto" {
		return false
	}

	t := common.GetGinToolValue(ctx)
	return t.Is("tasks", true)
}
