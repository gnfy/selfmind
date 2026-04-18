package identity

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// IdentityMapper 管理平台身份到全局用户的映射
type IdentityMapper struct {
	baseDir string
}

// NewIdentityMapper 创建一个身份映射器
func NewIdentityMapper(baseDir string) *IdentityMapper {
	return &IdentityMapper{baseDir: baseDir}
}

// getDB 获取 identity.db 的数据库连接
func (m *IdentityMapper) getDB() (*sql.DB, error) {
	dbPath := filepath.Join(m.baseDir, "identity.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_sync=NORMAL")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS identity_map (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		platform TEXT NOT NULL,
		platform_id TEXT NOT NULL,
		unified_uid TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(platform, platform_id)
	);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Resolve 根据 platform + platform_id 找到全局用户ID
// 如果该平台身份尚未绑定任何全局用户，返回空字符串
func (m *IdentityMapper) Resolve(ctx context.Context, platform, platformID string) (string, error) {
	db, err := m.getDB()
	if err != nil {
		return "", err
	}
	defer db.Close()

	var unifiedUID string
	err = db.QueryRowContext(ctx,
		"SELECT unified_uid FROM identity_map WHERE platform = ? AND platform_id = ?",
		platform, platformID,
	).Scan(&unifiedUID)

	if err == sql.ErrNoRows {
		return "", nil // 未绑定
	}
	if err != nil {
		return "", fmt.Errorf("identity resolve: %w", err)
	}
	return unifiedUID, nil
}

// Bind 将 platform+platform_id 绑定到已有的 unified_uid
func (m *IdentityMapper) Bind(ctx context.Context, platform, platformID, unifiedUID string) error {
	db, err := m.getDB()
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.ExecContext(ctx,
		`INSERT OR REPLACE INTO identity_map (platform, platform_id, unified_uid) VALUES (?, ?, ?)`,
		platform, platformID, unifiedUID,
	)
	if err != nil {
		return fmt.Errorf("identity bind: %w", err)
	}
	return nil
}

// GetPlatforms 列出某个 unified_uid 下所有已绑定的平台
func (m *IdentityMapper) GetPlatforms(ctx context.Context, unifiedUID string) ([]string, error) {
	db, err := m.getDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"SELECT platform FROM identity_map WHERE unified_uid = ?",
		unifiedUID,
	)
	if err != nil {
		return nil, fmt.Errorf("get platforms: %w", err)
	}
	defer rows.Close()

	var platforms []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		platforms = append(platforms, p)
	}
	return platforms, nil
}

// EnsureBound 尝试解析平台身份，如果未绑定则使用或创建 unified_uid
// 首次调用时自动创建新的 unified_uid 并绑定
func (m *IdentityMapper) EnsureBound(ctx context.Context, platform, platformID string) (string, error) {
	uid, err := m.Resolve(ctx, platform, platformID)
	if err != nil {
		return "", err
	}
	if uid != "" {
		return uid, nil // 已绑定
	}

	// 生成新的 unified_uid（简单用 platform+platformID 的哈希，实际用 UUID）
	uid = fmt.Sprintf("%s_%s", platform, platformID)
	err = m.Bind(ctx, platform, platformID, uid)
	if err != nil {
		return "", err
	}
	return uid, nil
}
