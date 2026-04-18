package memory

import (
	"context"
	"errors"
)

type contextKey string

const tenantKey contextKey = "tenantID"

// GetTenantID 从 context 中获取当前的租户ID
func GetTenantID(ctx context.Context) (string, error) {
	val, ok := ctx.Value(tenantKey).(string)
	if !ok || val == "" {
		return "", errors.New("tenantID not found in context")
	}
	return val, nil
}

// WithTenantID 将 tenantID 注入 context，用于后续的操作隔离
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey, tenantID)
}

// TenantScopedPath 确保路径只在用户专属目录下访问
func TenantScopedPath(baseDir string, tenantID string, relativePath string) string {
	return baseDir + "/" + tenantID + "/" + relativePath
}
