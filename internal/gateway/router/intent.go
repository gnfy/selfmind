package router

import (
	"regexp"
	"strings"
)

// Intent 表示用户意图分类
type Intent int

const (
	IntentContinue Intent = iota // 继续当前任务
	IntentTask                  // 需要执行、创建任务
	IntentSkill                 // 调用 skill（以 /skill 或 skill 名开头）
	IntentQuery                 // 知识库/历史查询
	IntentRoute                 // 平台路由指令（如切到微信）
	IntentCasual                // 闲聊、问答
)

// IntentClassifier 轻量级意图分类器（规则 + 简单正则）
type IntentClassifier struct {
	continuePatterns []*regexp.Regexp
	taskPatterns     []*regexp.Regexp
	skillPatterns    []*regexp.Regexp
	queryPatterns    []*regexp.Regexp
	routePatterns    []*regexp.Regexp
}

func NewIntentClassifier() *IntentClassifier {
	// 编译一次，后续复用
	return &IntentClassifier{
		continuePatterns: compilePatterns([]string{
			`继续`, `接着`, `上次的`, `刚才那个`,
			`\bcontinue\b`, `\bkeep going\b`, `\bgo on\b`,
			`再试`, `再搞`, `从头来`,
		}),
		taskPatterns: compilePatterns([]string{
			// 中文：帮我/请帮我 系列
			`帮我`, `请帮我`, `帮我做`, `帮我查`, `帮我看看`, `帮我找`,
			`帮我写`, `帮我改`, `帮我看看`, `帮我分析`,
			// 中文：执行/操作类
			`执行`, `创建`, `修改`, `写代码`, `写一个`,
			`查进度`, `部署`, `构建`, `运行`, `检查`, `生成`,
			`做一下`, `做完`, `搞一下`, `搞定`,
			// 中文：运维类
			`重启`, `停止`, `启动服务`, `查看日志`, `抓包`,
			// 中文：文件操作
			`新建文件`, `删除文件`, `编辑文件`, `读取文件`,
			// 中文：开发类
			`代码审查`, `跑测试`, `提交代码`, `合并分支`, `部署上线`,
			// 英文
			`\bcreate\b`, `\bexecute\b`, `\bdeploy\b`, `\brun\b`,
			`\bbuild\b`, `\bcheck\b`, `\bmodify\b`, `\bwrite code\b`,
			`\bgrep\b`, `\bgit\b`, `\bdocker\b`, `\bkubectl\b`,
			`\banalyze\b`, `\breview\b`, `\bfix\b`, `\bdebug\b`,
			// 问进度
			`进度`, `完成了`, `好了吗`, `搞定没`,
		}),
		skillPatterns: compilePatterns([]string{
			`^/skill\b`, `^/s\b`, `调用技能`,
			`用技能`, `执行技能`, `运行技能`,
		}),
		queryPatterns: compilePatterns([]string{
			`^/query\b`, `^/search\b`, `查一下`,
			`搜索.*历史`, `查历史`, `找之前的`,
			`记得.*吗`, `之前.*说过`,
		}),
		routePatterns: compilePatterns([]string{
			`^/route\b`, `切换到`, `切到`,
			`跳到微信`, `转到钉钉`,
		}),
	}
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	var result []*regexp.Regexp
	for _, p := range patterns {
		if p == "" {
			continue
		}
		// 不区分大小写
		re := regexp.MustCompile(`(?i)` + p)
		result = append(result, re)
	}
	return result
}

// Classify 根据关键词+正则判断用户意图
func (c *IntentClassifier) Classify(input string) Intent {
	// 1. 优先判断 Skill（显式 skill 命令）
	for _, re := range c.skillPatterns {
		if re.MatchString(input) {
			return IntentSkill
		}
	}

	// 2. 判断 Query（搜索历史/知识库）
	for _, re := range c.queryPatterns {
		if re.MatchString(input) {
			return IntentQuery
		}
	}

	// 3. 判断 Route（平台切换指令）
	for _, re := range c.routePatterns {
		if re.MatchString(input) {
			return IntentRoute
		}
	}

	// 4. 判断"继续"类
	for _, re := range c.continuePatterns {
		if re.MatchString(input) {
			return IntentContinue
		}
	}

	// 5. 判断"任务"类
	for _, re := range c.taskPatterns {
		if re.MatchString(input) {
			return IntentTask
		}
	}

	// 默认为闲聊
	return IntentCasual
}

// ClassifyWithReason 返回意图和简要说明
func (c *IntentClassifier) ClassifyWithReason(input string) (Intent, string) {
	intent := c.Classify(input)
	switch intent {
	case IntentSkill:
		return intent, "matched skill pattern"
	case IntentQuery:
		return intent, "matched query pattern"
	case IntentRoute:
		return intent, "matched route pattern"
	case IntentContinue:
		return intent, "matched continue pattern"
	case IntentTask:
		return intent, "matched task pattern"
	case IntentCasual:
		return intent, "no pattern matched, defaulting to casual"
	}
	return intent, "unknown"
}

// IsCasualShortQuestion 判断是否是简短的闲聊问题（不需要执行）
func IsCasualShortQuestion(input string) bool {
	lower := strings.ToLower(input)
	shortCasual := []string{
		"你好", "您好", "hi", "hello", "嗨", "hey",
		"谢谢", "多谢", "谢了",
		"再见", "拜拜", "bye", "晚安",
		"你是谁", "你叫什么", "你干嘛的",
		"今天天气", "现在几点",
		"牛逼", "厉害", "真棒",
	}
	for _, kw := range shortCasual {
		if lower == kw || strings.HasPrefix(lower, kw) {
			return true
		}
	}
	// 问"怎么样"、"如何"结尾的简单问句
	if strings.HasSuffix(lower, "怎么样") ||
		strings.HasSuffix(lower, "如何") ||
		strings.HasSuffix(lower, "吗") {
		return true
	}
	return false
}
