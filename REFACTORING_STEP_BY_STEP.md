# 重构执行方案：新建分支 + Cherry-pick

## 策略优势

✅ **更安全**：保留原 `oxsci-custom` 分支不动，随时可以回退
✅ **更清晰**：从干净的 main 开始，逐个 cherry-pick 定制功能
✅ **更灵活**：可以选择性地 pick commits，调整顺序
✅ **更容易测试**：每 pick 一个功能就测试一次

## 执行步骤

### Step 1: 备份和准备（5 分钟）

```bash
# 1.1 确保在正确的目录
cd /Users/t2wei/git/sub2api

# 1.2 查看当前状态
git status
git log --oneline -10

# 1.3 同步 main 分支到最新
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease

# 1.4 记录需要 cherry-pick 的 commits
git checkout oxsci-custom
git log main..oxsci-custom --oneline > /tmp/oxsci-commits.txt
cat /tmp/oxsci-commits.txt

# 预期输出：
# 50dc3102 refactor(logging): 使用开闭原则重构 LLM logging 功能
# 1f9051af doc: merge conflict docs
# 52b73a86 Merge upstream/main into oxsci-custom
# 5944f9ac feat: 添加 SUB2API_LOG_LEVEL 环境变量支持
# cb271df9 feat: env config for ecs and local, with proxy api support
# bd3274dc feat(logging): 添加 LLM 调用日志记录功能
# 75a23c53 feat(auth): 添加 OxSci OAuth2 登录支持
```

---

### Step 2: 创建新的重构分支（2 分钟）

```bash
# 2.1 基于最新 main 创建新分支
git checkout main
git checkout -b oxsci-custom-v2

# 2.2 验证分支干净
git log --oneline -3
# 应该显示 main 分支的最新 commits

# 2.3 推送到远程（可选，建议推送以备份）
git push origin oxsci-custom-v2
```

---

### Step 3: Cherry-pick 定制功能（按顺序）

#### 3.1 Pick OxSci OAuth2 登录功能

```bash
# Cherry-pick commit
git cherry-pick 75a23c53

# 如果有冲突，解决后：
# git add <resolved-files>
# git cherry-pick --continue

# 修改 commit message 添加前缀
git commit --amend

# 修改为：
# [OXSCI] feat(auth): 添加 OxSci OAuth2 登录支持
#
# - 新增 OxSci OAuth2 登录功能，实现与 OxSci 生态系统的统一认证
# - 支持 PKCE 流程和标准 OAuth2 授权码模式
# - 添加前端登录组件和回调页面
# - 扩展认证服务支持可信 OAuth 提供商自动注册
# - 配置文件新增 oxsci_oauth 配置项
#
# Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>

# 测试功能
echo "✅ 测试 OxSci OAuth2 功能是否正常"
```

#### 3.2 Pick LLM Logging 功能（原始版本）

```bash
# Cherry-pick commit
git cherry-pick bd3274dc

# 修改 commit message
git commit --amend

# 修改为：
# [OXSCI] feat(logging): 添加 LLM 调用日志记录功能
#
# - 添加 LLM 调用日志记录到 data-service
# - 记录请求/响应信息、token 使用量、耗时等
# - 支持通过配置启用/禁用
#
# 注：此版本将在后续 commit 中重构为 Hook 模式
#
# Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>

echo "✅ 测试 LLM Logging 功能是否正常"
```

#### 3.3 Pick Env Config 功能

```bash
# Cherry-pick commit
git cherry-pick cb271df9

# 修改 commit message
git commit --amend

# 修改为：
# [OXSCI] feat(config): 添加环境配置和 Proxy API 支持
#
# - 支持 ECS 和本地环境的不同配置
# - 为 LLM Log Client 和 OxSci OAuth 添加 API Key 支持
# - 用于通过 Lambda Proxy 访问内网服务
# - 更新配置示例和文档
#
# Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>

echo "✅ 测试 Env Config 功能是否正常"
```

#### 3.4 Pick SUB2API_LOG_LEVEL 功能

```bash
# Cherry-pick commit
git cherry-pick 5944f9ac

# 修改 commit message
git commit --amend

# 修改为：
# [OXSCI] feat(config): 添加 SUB2API_LOG_LEVEL 环境变量支持
#
# - 支持通过 SUB2API_LOG_LEVEL 环境变量动态设置日志级别
# - 使用独立的环境变量名避免与 data-service 冲突
# - 支持 DEBUG, INFO, WARN, ERROR 四个级别
#
# 注：此版本直接修改 main.go，将在后续 commit 中重构为配置后处理器
#
# Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>

echo "✅ 测试 LOG_LEVEL 环境变量是否生效"
```

#### 3.5 跳过 Merge commit 和文档 commit

```bash
# 不需要 cherry-pick 以下 commits：
# - 52b73a86 Merge upstream/main into oxsci-custom （merge commit）
# - 1f9051af doc: merge conflict docs （临时文档）

echo "跳过 merge commit 和临时文档"
```

#### 3.6 Pick LLM Logging 重构

```bash
# Cherry-pick commit
git cherry-pick 50dc3102

# 修改 commit message
git commit --amend

# 修改为：
# [OXSCI] refactor(logging): 使用开闭原则重构 LLM logging 功能
#
# - 引入 RequestLifecycleHook 接口，支持请求生命周期扩展
# - 将 LLM logging 改造为 LLMLoggingHook 实现
# - 降低对核心业务代码的侵入性
# - 便于添加更多扩展功能（如性能监控、审计日志等）
#
# 符合开闭原则：
# - 对扩展开放：可以添加新的 Hook 实现
# - 对修改封闭：核心业务逻辑保持稳定
#
# 详见：REFACTORING_LLM_LOGGING.md
#
# Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>

echo "✅ 测试 LLM Logging Hook 功能是否正常"
```

---

### Step 4: 检查当前状态（5 分钟）

```bash
# 4.1 查看 commit 历史
git log --oneline --graph -10

# 预期输出（干净的线性历史）：
# * xxxxxxx [OXSCI] refactor(logging): 使用开闭原则重构 LLM logging 功能
# * xxxxxxx [OXSCI] feat(config): 添加 SUB2API_LOG_LEVEL 环境变量支持
# * xxxxxxx [OXSCI] feat(config): 添加环境配置和 Proxy API 支持
# * xxxxxxx [OXSCI] feat(logging): 添加 LLM 调用日志记录功能
# * xxxxxxx [OXSCI] feat(auth): 添加 OxSci OAuth2 登录支持
# * 56a1e29c fix(gateway): 修复 SSE 流式响应 usage 统计错误
# * 45676fdc fix(ci): 转义 Telegram 消息中的 Markdown 特殊字符

# 4.2 查看与 main 的差异
git diff main --stat

# 4.3 运行测试
cd backend
go test ./...

# 4.4 本地启动验证
# make run
# 验证各个功能是否正常
```

---

### Step 5: 重构 SUB2API_LOG_LEVEL（P1 最高优先级）

#### 5.1 创建配置后处理器接口

```bash
# 创建目录
mkdir -p backend/internal/config/processors

# 创建文件
cat > backend/internal/config/post_processor.go << 'EOF'
package config

import (
	"context"
	"fmt"
	"sort"
)

// ConfigPostProcessor 配置加载后的处理器接口
type ConfigPostProcessor interface {
	// Process 处理配置，返回修改后的配置
	Process(ctx context.Context, cfg *Config) (*Config, error)

	// Priority 返回处理优先级（数字越小越先执行）
	Priority() int
}

// ConfigPostProcessorChain 配置后处理器链
type ConfigPostProcessorChain struct {
	processors []ConfigPostProcessor
}

// NewConfigPostProcessorChain 创建配置后处理器链
func NewConfigPostProcessorChain() *ConfigPostProcessorChain {
	return &ConfigPostProcessorChain{
		processors: make([]ConfigPostProcessor, 0),
	}
}

// Register 注册一个后处理器
func (c *ConfigPostProcessorChain) Register(processor ConfigPostProcessor) {
	c.processors = append(c.processors, processor)
	// 按优先级排序
	sort.Slice(c.processors, func(i, j int) bool {
		return c.processors[i].Priority() < c.processors[j].Priority()
	})
}

// Process 按顺序执行所有后处理器
func (c *ConfigPostProcessorChain) Process(ctx context.Context, cfg *Config) (*Config, error) {
	for _, processor := range c.processors {
		var err error
		cfg, err = processor.Process(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("config post-processor %T failed: %w", processor, err)
		}
	}
	return cfg, nil
}
EOF
```

#### 5.2 创建日志级别环境变量处理器

```bash
cat > backend/internal/config/processors/log_level_env_processor.go << 'EOF'
package processors

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// LogLevelEnvProcessor 从 SUB2API_LOG_LEVEL 环境变量读取日志级别
type LogLevelEnvProcessor struct{}

// NewLogLevelEnvProcessor 创建日志级别环境变量处理器
func NewLogLevelEnvProcessor() *LogLevelEnvProcessor {
	return &LogLevelEnvProcessor{}
}

// Priority 返回优先级（10 表示较早执行）
func (p *LogLevelEnvProcessor) Priority() int {
	return 10
}

// Process 处理配置
func (p *LogLevelEnvProcessor) Process(ctx context.Context, cfg *config.Config) (*config.Config, error) {
	logLevel := os.Getenv("SUB2API_LOG_LEVEL")
	if logLevel == "" {
		return cfg, nil
	}

	logLevel = strings.ToUpper(strings.TrimSpace(logLevel))

	var zapLevel string
	switch logLevel {
	case "DEBUG":
		zapLevel = "debug"
	case "INFO":
		zapLevel = "info"
	case "WARN":
		zapLevel = "warn"
	case "ERROR":
		zapLevel = "error"
	default:
		log.Printf("[WARN] Invalid SUB2API_LOG_LEVEL value: %s (expected: DEBUG, INFO, WARN, ERROR)", logLevel)
		return cfg, nil
	}

	log.Printf("[INFO] SUB2API_LOG_LEVEL=%s, setting log.level to %s", logLevel, zapLevel)
	cfg.Log.Level = zapLevel
	return cfg, nil
}
EOF
```

#### 5.3 修改 main.go

```bash
# 先查看当前的 main.go 中 SUB2API_LOG_LEVEL 的实现
git diff HEAD~1 backend/cmd/server/main.go

# 手动编辑 main.go，将环境变量逻辑替换为后处理器
# 需要：
# 1. 导入 "github.com/Wei-Shaw/sub2api/internal/config/processors"
# 2. 删除原来的 23 行环境变量逻辑
# 3. 添加 3 行后处理器调用

echo "请手动编辑 backend/cmd/server/main.go"
echo "参考 REFACTORING_IMPLEMENTATION_PLAN.md 中的 Phase 2.3"
```

#### 5.4 测试和提交

```bash
# 测试
cd backend
go mod tidy
go test ./internal/config/...

# 测试环境变量是否生效
SUB2API_LOG_LEVEL=DEBUG go run cmd/server/main.go

# 提交
git add backend/internal/config/post_processor.go
git add backend/internal/config/processors/
git add backend/cmd/server/main.go

git commit -m "[OXSCI] refactor(config): 使用配置后处理器重构 SUB2API_LOG_LEVEL

- 引入 ConfigPostProcessor 接口，支持插件化配置处理
- 将 main.go 中的日志级别环境变量逻辑抽取为 LogLevelEnvProcessor
- 降低 main.go 的修改（从 23 行减少到 3 行），减少与 upstream 的冲突风险

符合开闭原则：
- 对扩展开放：可以添加新的配置后处理器
- 对修改封闭：main.go 只需添加 3 行代码

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Step 6: 重构 OxSci OAuth2（P2 高优先级）

#### 6.1 创建 OAuth Provider 接口

```bash
mkdir -p backend/internal/auth/providers

cat > backend/internal/auth/oauth_provider.go << 'EOF'
package auth

import (
	"context"
	"github.com/gin-gonic/gin"
)

// OAuthProvider OAuth 提供商接口
type OAuthProvider interface {
	// Name 返回提供商名称（如 "oxsci", "linuxdo"）
	Name() string

	// StartAuth 启动 OAuth 流程
	StartAuth(c *gin.Context)

	// HandleCallback 处理 OAuth 回调
	HandleCallback(c *gin.Context)

	// IsEnabled 返回是否启用
	IsEnabled(ctx context.Context) bool
}

// OAuthProviderRegistry OAuth 提供商注册表
type OAuthProviderRegistry struct {
	providers map[string]OAuthProvider
}

// NewOAuthProviderRegistry 创建 OAuth 提供商注册表
func NewOAuthProviderRegistry() *OAuthProviderRegistry {
	return &OAuthProviderRegistry{
		providers: make(map[string]OAuthProvider),
	}
}

// Register 注册一个 OAuth 提供商
func (r *OAuthProviderRegistry) Register(provider OAuthProvider) {
	r.providers[provider.Name()] = provider
}

// GetProvider 获取指定名称的提供商
func (r *OAuthProviderRegistry) GetProvider(name string) (OAuthProvider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// AllProviders 返回所有已注册的提供商
func (r *OAuthProviderRegistry) AllProviders() []OAuthProvider {
	var result []OAuthProvider
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}
EOF
```

#### 6.2 将 OxSci OAuth 改造为 Provider

```bash
# 复制原文件到新位置
cp backend/internal/handler/auth_oxsci_oauth.go backend/internal/auth/providers/oxsci_provider.go

# 手动编辑 oxsci_provider.go，改造为实现 OAuthProvider 接口
echo "请手动编辑 backend/internal/auth/providers/oxsci_provider.go"
echo "参考 CUSTOM_FEATURES_ANALYSIS.md 中的重构建议"
echo ""
echo "主要修改："
echo "1. package handler -> package providers"
echo "2. 添加 OxSciOAuthProvider 结构体"
echo "3. 实现 OAuthProvider 接口的 4 个方法"
echo "4. 将原 AuthHandler 的方法改为 OxSciOAuthProvider 的方法"
```

#### 6.3 修改路由注册

```bash
# 手动编辑 backend/internal/server/routes/auth.go
echo "请手动编辑 backend/internal/server/routes/auth.go"
echo "参考 CUSTOM_FEATURES_ANALYSIS.md 中的重构建议"
echo ""
echo "主要修改："
echo "1. 添加 oauthRegistry *auth.OAuthProviderRegistry 参数"
echo "2. 删除硬编码的 OxSci OAuth 路由"
echo "3. 添加动态注册循环"
```

#### 6.4 更新依赖注入（使用 Wire）

```bash
# 编辑 backend/cmd/server/wire.go
echo "请手动编辑 backend/cmd/server/wire.go"
echo "添加 OAuth Provider 相关的依赖注入"

# 重新生成 wire 代码
cd backend
go install github.com/google/wire/cmd/wire@latest
cd cmd/server
wire

# 测试编译
cd ../..
go build -o /tmp/sub2api ./cmd/server
```

#### 6.5 测试和提交

```bash
# 测试
cd backend
go test ./internal/auth/...

# 提交
git add backend/internal/auth/
git add backend/internal/server/routes/auth.go
git add backend/cmd/server/

# 如果保留旧文件，删除它
git rm backend/internal/handler/auth_oxsci_oauth.go

git commit -m "[OXSCI] refactor(auth): 使用 Provider 模式重构 OAuth 功能

- 引入 OAuthProvider 接口，支持插件化 OAuth 提供商
- 将 OxSci OAuth 实现改造为 OxSciOAuthProvider
- 路由注册改为动态扫描所有启用的 Provider
- 降低路由层的修改，减少与 upstream 的冲突风险

符合开闭原则：
- 对扩展开放：可以添加新的 OAuth Provider（如 Google, GitHub）
- 对修改封闭：routes/auth.go 只需添加 5 行循环代码

未来可以轻松添加新的 OAuth Provider：
- LinuxDo OAuth（已存在，可后续改造）
- GitHub OAuth
- Google OAuth

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Step 7: 最终验证和切换（15 分钟）

```bash
# 7.1 查看最终的 commit 历史
git log --oneline --graph -10

# 预期输出：
# * [OXSCI] refactor(auth): 使用 Provider 模式重构 OAuth 功能
# * [OXSCI] refactor(config): 使用配置后处理器重构 SUB2API_LOG_LEVEL
# * [OXSCI] refactor(logging): 使用开闭原则重构 LLM logging 功能
# * [OXSCI] feat(config): 添加 SUB2API_LOG_LEVEL 环境变量支持
# * [OXSCI] feat(config): 添加环境配置和 Proxy API 支持
# * [OXSCI] feat(logging): 添加 LLM 调用日志记录功能
# * [OXSCI] feat(auth): 添加 OxSci OAuth2 登录支持
# * 56a1e29c fix(gateway): 修复 SSE 流式响应 usage 统计错误

# 7.2 完整测试
cd backend
go test ./...
go build -o /tmp/sub2api ./cmd/server

# 7.3 本地运行验证所有功能
# /tmp/sub2api

# 7.4 推送新分支
git push origin oxsci-custom-v2

# 7.5 切换默认分支（可选，谨慎操作）
# 选项 A：保留两个分支，观察一段时间
echo "两个分支并存，观察 oxsci-custom-v2 的稳定性"

# 选项 B：直接替换（推荐先备份）
git checkout oxsci-custom
git reset --hard oxsci-custom-v2
git push origin oxsci-custom --force-with-lease

# 选项 C：重命名分支
git branch -m oxsci-custom oxsci-custom-old
git branch -m oxsci-custom-v2 oxsci-custom
git push origin oxsci-custom --force-with-lease
git push origin :oxsci-custom-v2  # 删除远程 v2 分支
```

---

## 时间估算

| 步骤 | 任务 | 预估时间 |
|------|------|----------|
| Step 1 | 备份和准备 | 5 分钟 |
| Step 2 | 创建新分支 | 2 分钟 |
| Step 3 | Cherry-pick 功能 | 20 分钟 |
| Step 4 | 检查状态 | 5 分钟 |
| Step 5 | 重构 SUB2API_LOG_LEVEL | 1.5 小时 |
| Step 6 | 重构 OxSci OAuth2 | 2.5 小时 |
| Step 7 | 最终验证 | 15 分钟 |
| **总计** | | **约 5 小时** |

## Cherry-pick 冲突处理

如果在 cherry-pick 过程中遇到冲突：

```bash
# 1. 查看冲突文件
git status

# 2. 手动解决冲突
# 编辑冲突文件，保留正确的代码

# 3. 标记为已解决
git add <resolved-files>

# 4. 继续 cherry-pick
git cherry-pick --continue

# 如果要放弃当前 cherry-pick
git cherry-pick --abort
```

## 常见问题

### Q: Cherry-pick 时提示 "empty commit"？
A: 这个 commit 的修改已经存在于 main 分支中，跳过即可：
```bash
git cherry-pick --skip
```

### Q: 要不要保留原来的 oxsci-custom 分支？
A: 建议保留 1-2 周，确保新分支稳定后再删除。

### Q: 新分支如何与 upstream 保持同步？
A: 使用 rebase：
```bash
git checkout main
git fetch upstream
git reset --hard upstream/main
git checkout oxsci-custom-v2  # 或新的 oxsci-custom
git rebase main
```

## 下一步

重构完成后：
1. ✅ 更新 README.md，说明 OxSci 定制功能
2. ✅ 编写 CONTRIBUTING_OXSCI.md，指导如何添加新功能
3. ✅ 创建 GitHub PR 模板（如果需要向 upstream 贡献）
4. ✅ 建立 CI/CD 自动化测试流程
