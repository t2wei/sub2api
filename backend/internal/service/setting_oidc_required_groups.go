package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// OIDC 登录要求的组。
//
// oidc_connect_required_groups 存一个 JSON 数组，每项是一个组名或前缀。非空时，OIDC 登录
// 只有在 IdP 的 groups 声明里至少一项与某个条目匹配时才放行：以 "*" 结尾的条目按前缀匹配
// （"staff.*" 匹配 "staff.basic"），其余条目要求与组名完全相同（区分大小写）。没有 groups
// 声明等同于没有任何组。空数组（缺省）表示不限，登录行为和没有这个设置时完全一样。
//
// 只作用于 OIDC 登录回调（新建账号、登录已有账号、绑定都在内）；已签发的会话与 API Key
// 不受影响。由专用接口读写，不进 SystemSettings。

// SettingKeyOIDCConnectRequiredGroups 登录要求的组（JSON 数组）。
const SettingKeyOIDCConnectRequiredGroups = "oidc_connect_required_groups"

// MaxOIDCRequiredGroups 是条目数上限。
const MaxOIDCRequiredGroups = 50

const oidcRequiredGroupMaxLen = 128

// ErrOIDCRequiredGroupsCorrupt 已保存的值无法解析。登录回调把它当作拒绝（fail closed），
// 管理员经 PUT 接口重新保存即可恢复。
var ErrOIDCRequiredGroupsCorrupt = infraerrors.InternalServer(
	"OIDC_REQUIRED_GROUPS_CORRUPT",
	"stored oidc required groups cannot be parsed",
)

// normalizeOIDCRequiredGroupEntry 去首尾空白并校验一条条目：不含空白与控制字符，"*" 只能
// 作为最后一个字符出现，长度不超过上限。
func normalizeOIDCRequiredGroupEntry(raw string) (string, bool) {
	entry := strings.TrimSpace(raw)
	if entry == "" || len(entry) > oidcRequiredGroupMaxLen {
		return "", false
	}
	for _, r := range entry {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	if star := strings.IndexByte(entry, '*'); star >= 0 && star != len(entry)-1 {
		return "", false
	}
	return entry, true
}

// NormalizeOIDCRequiredGroups 校验并归一管理员提交的条目：跳过空行、去重并保持原有顺序；
// 任一条目不合法或超过条数上限都整体拒绝。
func NormalizeOIDCRequiredGroups(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	var invalid []string
	for _, item := range raw {
		if strings.TrimSpace(item) == "" {
			continue
		}
		entry, ok := normalizeOIDCRequiredGroupEntry(item)
		if !ok {
			invalid = append(invalid, strings.TrimSpace(item))
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	if len(invalid) > 0 {
		return nil, infraerrors.BadRequest(
			"OIDC_REQUIRED_GROUPS_INVALID_ENTRY",
			fmt.Sprintf("invalid group entry (%d): %s", len(invalid), strings.Join(invalid, ", ")),
		).WithMetadata(map[string]string{
			"invalid_count": strconv.Itoa(len(invalid)),
		})
	}
	if len(out) > MaxOIDCRequiredGroups {
		return nil, infraerrors.BadRequest(
			"OIDC_REQUIRED_GROUPS_TOO_MANY",
			fmt.Sprintf("required groups cannot exceed %d entries", MaxOIDCRequiredGroups),
		).WithMetadata(map[string]string{
			"max": strconv.Itoa(MaxOIDCRequiredGroups),
		})
	}
	return out, nil
}

// parseOIDCRequiredGroups 解析已保存的值。空值即不限；JSON 不合法或含不合法条目时返回
// ErrOIDCRequiredGroupsCorrupt，不静默放宽。
func parseOIDCRequiredGroups(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, ErrOIDCRequiredGroupsCorrupt.WithCause(err)
	}
	groups, err := NormalizeOIDCRequiredGroups(items)
	if err != nil {
		return nil, ErrOIDCRequiredGroupsCorrupt.WithCause(err)
	}
	return groups, nil
}

// OIDCGroupsSatisfy 判断 IdP 给的 groups 是否满足要求：required 为空即满足；否则至少一个
// 组与某个条目匹配（见文件头）。
func OIDCGroupsSatisfy(required, groups []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, pattern := range required {
		prefix, isPrefix := strings.CutSuffix(pattern, "*")
		for _, group := range groups {
			if isPrefix {
				if strings.HasPrefix(group, prefix) {
					return true
				}
			} else if group == pattern {
				return true
			}
		}
	}
	return false
}

// GetOIDCRequiredGroups 返回已保存的登录要求的组；未设置时为空（不限）。
func (s *SettingService) GetOIDCRequiredGroups(ctx context.Context) ([]string, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyOIDCConnectRequiredGroups)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("get oidc required groups: %w", err)
	}
	return parseOIDCRequiredGroups(raw)
}

// SetOIDCRequiredGroups 整体替换登录要求的组，返回保存后的值；传空数组即取消限制。
// 每次登录都直读，保存后下一次登录即生效（不经缓存）。
func (s *SettingService) SetOIDCRequiredGroups(ctx context.Context, groups []string) ([]string, error) {
	normalized, err := NormalizeOIDCRequiredGroups(groups)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal oidc required groups: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyOIDCConnectRequiredGroups, string(data)); err != nil {
		return nil, fmt.Errorf("save oidc required groups: %w", err)
	}
	return normalized, nil
}
