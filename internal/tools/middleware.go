package tools

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// AuthMiddleware 检查执行权限
// tenantID 由 TenantIsolationMiddleware 注入到 args["_tenant_id"]
func AuthMiddleware(mem interface {
	GetPermission(ctx context.Context, tenantID, toolName string) (bool, error)
}) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			tenantID, _ := args["_tenant_id"].(string)
			toolName, _ := args["_tool_name"].(string)

			if tenantID == "" {
				return "", fmt.Errorf("[Auth] tenantID not found in args, ensure TenantIsolationMiddleware is applied")
			}

			if mem != nil {
				allowed, err := mem.GetPermission(context.Background(), tenantID, toolName)
				if err == nil && !allowed {
					return "", fmt.Errorf("[Auth] tenant %s is not allowed to use tool %s", tenantID, toolName)
				}
			}

			return next(args)
		}
	}
}

// ApprovalMiddleware 检查是否需要人工审批
// 当 dry_run=true 时不真正执行，只返回待审批状态
func ApprovalMiddleware(dryRun bool) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			if dryRun {
				return "", fmt.Errorf("[Approval] dry_run=true, tool execution pending approval")
			}
			log.Printf("[Approval] Running tool\n")
			return next(args)
		}
	}
}

// RateLimitMiddleware 简单的速率限制中间件
type RateLimitMiddleware struct {
	maxCalls int
	count    int
}

func RateLimit(maxCalls int) *RateLimitMiddleware {
	return &RateLimitMiddleware{maxCalls: maxCalls}
}

func (r *RateLimitMiddleware) Middleware(next ToolExecutor) ToolExecutor {
	return func(args map[string]interface{}) (string, error) {
		if r.count >= r.maxCalls {
			return "", fmt.Errorf("rate limit exceeded: max %d calls", r.maxCalls)
		}
		r.count++
		return next(args)
	}
}

// LoggingMiddleware 记录工具执行的日志中间件
func LoggingMiddleware(next ToolExecutor) ToolExecutor {
	return func(args map[string]interface{}) (string, error) {
		log.Printf("[Tools] Executing with args: %s\n", MarshalArgs(args))
		result, err := next(args)
		if err != nil {
			log.Printf("[Tools] Error: %v\n", err)
		} else {
			log.Printf("[Tools] Result: %s\n", result)
		}
		return result, err
	}
}

// TenantIsolationMiddleware 确保工具只能访问本租户的数据
// 注入 tenantID 到 args 中
func TenantIsolationMiddleware(tenantID string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			// 强制覆盖 tenantID，防止跨租户访问
			args["_tenant_id"] = tenantID
			return next(args)
		}
	}
}

// EnvVarMiddleware 检查必要的环境变量
func EnvVarMiddleware(requiredVars ...string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			for _, v := range requiredVars {
				if os.Getenv(v) == "" {
					return "", fmt.Errorf("missing required environment variable: %s", v)
				}
			}
			return next(args)
		}
	}
}

// SmartApprovalMiddleware 检查危险操作并请求人工审批
func SmartApprovalMiddleware(projectRoot string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)
			dangerous := false
			reason := ""

			// 1. Shell 匹配
			if toolName == "execute_command" {
				cmd, _ := args["command"].(string)
				dangerousCommands := []string{"rm ", "> /dev/", "chmod ", "chown ", "kill ", "pkill ", "shutdown "}
				for _, dc := range dangerousCommands {
					if strings.Contains(cmd, dc) {
						dangerous = true
						reason = fmt.Sprintf("contains dangerous command: %s", dc)
						break
					}
				}
			}

			// 2. 文件系统匹配
			path, _ := args["path"].(string)
			if path != "" {
				if strings.Contains(path, "/etc/") || strings.Contains(path, "/root/") || strings.Contains(path, "/dev/") {
					dangerous = true
					reason = fmt.Sprintf("accesses restricted path: %s", path)
				} else if filepath.IsAbs(path) && !strings.HasPrefix(path, projectRoot) {
					dangerous = true
					reason = fmt.Sprintf("accesses path outside project root: %s", path)
				}
			}

			if dangerous && ClarifyFn != nil {
				question := fmt.Sprintf("⚠️ 发现危险操作提示！\n工具: %s\n参数: %v\n原因: %s\n是否确认执行？", toolName, MarshalArgs(args), reason)
				response := ClarifyFn(question, []string{"Yes", "No"})
				if strings.ToLower(response) != "yes" {
					return "", fmt.Errorf("操作已被用户取消")
				}
			}

			return next(args)
		}
	}
}
