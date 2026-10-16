package proxy

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const agentTitleTopicMaxRunes = 31

var (
	agentTitleNow          = time.Now
	agentTitlePrefixRe     = regexp.MustCompile(`^\d{4}\s*\|`)
	agentTitleIdentifierRe = regexp.MustCompile(`(?i)\b(?:MUL|COC)-\d+\b|(?:^|[\s(（\[])([#!]\d+)\b`)
	agentLocalCommandTagRe = regexp.MustCompile(`(?is)<(?:local-command-caveat|command-name|command-args|local-command-stdout)>.*?</(?:local-command-caveat|command-name|command-args|local-command-stdout)>`)
)

var agentTitleCategories = []struct {
	name     string
	keywords []string
}{
	{"调试", []string{"bug", "fix", "error", "fail", "panic", "报错", "故障", "异常", "修复", "挂了", "超时", "恢复", "调试", "debug", "排障"}},
	{"发布", []string{"release", "deploy", "发布", "上线", "发版", "合并", "merge"}},
	{"优化", []string{"optim", "perf", "性能", "优化", "提速", "缓存", "耗时", "瘦身"}},
	{"整改", []string{"refactor", "重构", "清理", "整改", "治理", "规范", "收敛"}},
	{"设计", []string{"design", "spec", "架构", "方案", "设计", "协议", "选型", "评审"}},
	{"配置", []string{"config", "yaml", "权限", "账号", "额度", "配置", "环境变量", "启动器", "密钥", "cookie", "token"}},
	{"文档", []string{"readme", "文档", "说明", "注释", "指南", "日报", "周报", "wiki", "归档", "doc"}},
	{"分析", []string{"research", "analyze", "调研", "分析", "检索", "排查", "对比", "评估", "查一下", "看看", "为什么"}},
	{"功能", []string{"feat", "add", "support", "新增", "支持", "实现", "功能", "接入", "开发", "接口", "命名"}},
}

func isValidAgentTitleCategory(cat string) bool {
	cat = strings.TrimSpace(cat)
	if cat == "杂项" {
		return true
	}
	for _, c := range agentTitleCategories {
		if c.name == cat {
			return true
		}
	}
	return false
}

func inferAgentTitleCategory(text string) string {
	lower := strings.ToLower(text)
	for _, c := range agentTitleCategories {
		for _, kw := range c.keywords {
			if strings.Contains(lower, kw) {
				return c.name
			}
		}
	}
	return "杂项"
}

func cleanAgentTitleSource(raw string) string {
	s := normalizeSessionUserContent(raw)
	s = agentLocalCommandTagRe.ReplaceAllString(s, " ")
	if idx := strings.Index(s, "\n\nStructured output format:\n"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func extractAgentTitleIdentifier(text string) string {
	m := agentTitleIdentifierRe.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return strings.TrimSpace(m[0])
}

func extractAgentTitleTopic(raw string) string {
	cleaned := cleanAgentTitleSource(raw)
	if cleaned == "" {
		return ""
	}
	id := extractAgentTitleIdentifier(cleaned)
	var firstLine string
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#-*>\t ")
		line = strings.TrimSpace(line)
		if line != "" {
			firstLine = line
			break
		}
	}
	if firstLine == "" {
		return ""
	}
	topic := strings.Join(strings.Fields(firstLine), " ")
	if id != "" && !strings.Contains(strings.ToLower(topic), strings.ToLower(id)) {
		topic = id + " " + topic
	}
	return truncateAgentRunes(topic, agentTitleTopicMaxRunes)
}

func truncateAgentRunes(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	trimmed := strings.TrimRightFunc(string(runes[:max]), func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，。、：；,.|-_", r)
	})
	if trimmed == "" {
		return string(runes[:max])
	}
	return trimmed
}

func subagentChannelTag(subagentID string) string {
	id := strings.TrimSpace(subagentID)
	if id == "" {
		return "子"
	}
	if strings.HasPrefix(id, "subagent:") {
		id = strings.TrimPrefix(id, "subagent:")
	} else if strings.HasPrefix(strings.ToLower(id), "agent-") {
		id = id[6:]
	}
	var short []rune
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			short = append(short, r)
			if len(short) == 4 {
				break
			}
		}
	}
	if len(short) == 0 {
		return "子"
	}
	return "子#" + string(short)
}

func formatAgentThreadTitle(channel, category, topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return ""
	}
	if !isValidAgentTitleCategory(category) {
		category = "杂项"
	}
	return fmt.Sprintf("%s | %s | %s | %s", agentTitleNow().Format("0102"), channel, category, topic)
}

func buildCloudAgentTitle(customTitle, customCategory, task string) string {
	if t := strings.TrimSpace(customTitle); agentTitlePrefixRe.MatchString(t) {
		return truncateAgentRunes(t, 80)
	}
	source := task
	if strings.TrimSpace(customTitle) != "" {
		source = customTitle
	}
	cat := strings.TrimSpace(customCategory)
	if !isValidAgentTitleCategory(cat) {
		cat = inferAgentTitleCategory(source + "\n" + task)
	}
	topic := extractAgentTitleTopic(source)
	if topic == "" {
		topic = "云端任务"
	}
	return formatAgentThreadTitle("云", cat, topic)
}

func agentTurnTitle(in agentPlanInput) string {
	if in.Recap {
		return formatAgentThreadTitle("旁", "杂项", "暂离回归回顾")
	}
	ns := nonSystemMessages(in.Messages)
	if len(ns) == 0 {
		return ""
	}
	if kind, _ := classifySummaryInstruction(ns[len(ns)-1]); kind == summaryKnown {
		return formatAgentThreadTitle("旁", "杂项", "上下文压缩摘要")
	}
	channel := "本"
	if in.SubagentID != "" {
		channel = subagentChannelTag(in.SubagentID)
	} else if !in.Tooled {
		channel = "旁"
	}
	for _, m := range ns {
		if m.Role != "user" || m.ToolCallID != "" {
			continue
		}
		cleaned := cleanAgentTitleSource(m.Content)
		if cleaned == "" {
			continue
		}
		topic := extractAgentTitleTopic(m.Content)
		if topic == "" {
			continue
		}
		return formatAgentThreadTitle(channel, inferAgentTitleCategory(cleaned), topic)
	}
	return ""
}

// user_set_title prevents Notion's async auto-titler from overwriting this title with protocol boilerplate.
func setAgentThreadTitle(ctx context.Context, client *http.Client, acc *Account, threadID, title string) {
	if acc == nil || acc.SpaceID == "" || threadID == "" || title == "" {
		return
	}
	body := map[string]interface{}{
		"requestId": generateUUIDv4(),
		"transactions": []interface{}{
			map[string]interface{}{
				"id":      generateUUIDv4(),
				"spaceId": acc.SpaceID,
				"debug": map[string]interface{}{
					"userAction":         "agentChat.threadPersistenceActions.updateThreadTitle",
					"clientCommitTimeMs": agentTitleNow().UnixMilli(),
				},
				"operations": []interface{}{
					map[string]interface{}{
						"pointer": map[string]interface{}{
							"table":   "thread",
							"spaceId": acc.SpaceID,
							"id":      threadID,
						},
						"path":    []string{"data"},
						"command": "update",
						"args": map[string]interface{}{
							"title":          title,
							"user_set_title": true,
						},
					},
				},
			},
		},
	}
	_ = agentPost(ctx, client, acc, "saveTransactionsFanout", body, nil)
}
