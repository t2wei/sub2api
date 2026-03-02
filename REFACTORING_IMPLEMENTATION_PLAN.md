# OxSci 定制功能重构实施计划

## 背景

当前 `oxsci-custom` 分支包含 4 个定制功能，其中 3 个需要重构以符合开闭原则，降低与 upstream 合并的冲突风险。

## 总体目标

1. **清理 commit 历史**：使用 rebase 保持定制功能始终基于最新 upstream
2. **重构高风险代码**：将侵入性修改改造为插件化扩展
3. **文档化定制功能**：便于未来维护和演进

## 实施步骤

### Phase 0: 备份当前状态（预估时间：5 分钟）

```bash
# 创建备份分支
git checkout oxsci-custom
git checkout -b oxsci-custom-backup
git push origin oxsci-custom-backup

# 记录当前状态
git log --oneline -10 > backup-commits.txt
```

---

### Phase 1: 清理 Commit 历史（预估时间：30 分钟）

**目标：** 将定制功能的 commits 整理到 upstream 最新版本之上

#### Step 1.1: 同步 main 分支

```bash
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease
```

#### Step 1.2: 交互式 Rebase

```bash
git checkout oxsci-custom
git rebase -i main
```

在交互式编辑器中，调整 commit 顺序和内容：

```
pick 75a23c53 feat(auth): 添加 OxSci OAuth2 登录支持
pick cb271df9 feat: env config for ecs and local, with proxy api support
pick bd3274dc feat(logging): 添加 LLM 调用日志记录功能
pick 5944f9ac feat: 添加 SUB2API_LOG_LEVEL 环境变量支持
squash 1f9051af doc: merge conflict docs  # 合并到上一个 commit
pick 50dc3102 refactor(logging): 使用开闭原则重构 LLM logging 功能
```

#### Step 1.3: 添加统一前缀

使用 `git commit --amend` 为每个 commit 添加 `[OXSCI]` 前缀：

```bash
git rebase -i main

# 将每个 commit 改为 reword
reword 75a23c53 [OXSCI] feat(auth): 添加 OxSci OAuth2 登录支持
reword cb271df9 [OXSCI] feat: env config for ecs and local, with proxy api support
# ...
```

---

### Phase 2: 重构 SUB2API_LOG_LEVEL (P1 - 最高优先级)

**目标：** 将 `main.go` 中的环境变量逻辑抽取为配置后处理器

#### Step 2.1: 创建配置后处理器接口

创建文件：`backend/internal/config/post_processor.go`

```go
package config

import (
	"context"
	"sort"
)

type ConfigPostProcessor interface {
	Process(ctx context.Context, cfg *Config) (*Config, error)
	Priority() int
}

type ConfigPostProcessorChain struct {
	processors []ConfigPostProcessor
}

func NewConfigPostProcessorChain() *ConfigPostProcessorChain {
	return &ConfigPostProcessorChain{
		processors: make([]ConfigPostProcessor, 0),
	}
}

func (c *ConfigPostProcessorChain) Register(processor ConfigPostProcessor) {
	c.processors = append(c.processors, processor)
	sort.Slice(c.processors, func(i, j int) bool {
		return c.processors[i].Priority() < c.processors[j].Priority()
	})
}

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
```

#### Step 2.2: 创建日志级别处理器

创建文件：`backend/internal/config/processors/log_level_env_processor.go`

```go
package processors

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type LogLevelEnvProcessor struct{}

func NewLogLevelEnvProcessor() *LogLevelEnvProcessor {
	return &LogLevelEnvProcessor{}
}

func (p *LogLevelEnvProcessor) Priority() int {
	return 10
}

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
```

#### Step 2.3: 修改 main.go

修改文件：`backend/cmd/server/main.go`

移除原有的 23 行环境变量逻辑，替换为：

```go
import (
	// ... 原有 imports
	"github.com/Wei-Shaw/sub2api/internal/config/processors"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// [OXSCI] 应用配置后处理器
	postProcessorChain := config.NewConfigPostProcessorChain()
	postProcessorChain.Register(processors.NewLogLevelEnvProcessor())

	cfg, err = postProcessorChain.Process(context.Background(), cfg)
	if err != nil {
		log.Fatalf("Failed to process config: %v", err)
	}

	// ... 原有代码
}
```

#### Step 2.4: 提交重构

```bash
git add backend/internal/config/post_processor.go
git add backend/internal/config/processors/log_level_env_processor.go
git add backend/cmd/server/main.go

git commit -m "[OXSCI] refactor(config): 使用配置后处理器重构 SUB2API_LOG_LEVEL

- 引入 ConfigPostProcessor 接口，支持插件化配置处理
- 将 main.go 中的日志级别环境变量逻辑抽取为 LogLevelEnvProcessor
- 降低 main.go 的修改，减少与 upstream 的冲突风险

符合开闭原则：
- 对扩展开放：可以添加新的配置后处理器
- 对修改封闭：main.go 只需添加 3 行代码

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Phase 3: 重构 OxSci OAuth2 (P2 - 高优先级)

**目标：** 将 OAuth 实现改造为插件化架构

#### Step 3.1: 创建 OAuth Provider 接口

创建文件：`backend/internal/auth/oauth_provider.go`

```go
package auth

import (
	"context"
	"github.com/gin-gonic/gin"
)

type OAuthProvider interface {
	Name() string
	StartAuth(c *gin.Context)
	HandleCallback(c *gin.Context)
	IsEnabled(ctx context.Context) bool
}

type OAuthProviderRegistry struct {
	providers map[string]OAuthProvider
}

func NewOAuthProviderRegistry() *OAuthProviderRegistry {
	return &OAuthProviderRegistry{
		providers: make(map[string]OAuthProvider),
	}
}

func (r *OAuthProviderRegistry) Register(provider OAuthProvider) {
	r.providers[provider.Name()] = provider
}

func (r *OAuthProviderRegistry) GetProvider(name string) (OAuthProvider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

func (r *OAuthProviderRegistry) AllProviders() []OAuthProvider {
	var result []OAuthProvider
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}
```

#### Step 3.2: 将 OxSci OAuth 改造为 Provider

重命名：`backend/internal/handler/auth_oxsci_oauth.go` → `backend/internal/auth/providers/oxsci_provider.go`

并修改为实现 `OAuthProvider` 接口：

```go
package providers

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/auth"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	// ... 其他 imports
)

type OxSciOAuthProvider struct {
	cfg         *config.Config
	authService *service.AuthService
}

func NewOxSciOAuthProvider(cfg *config.Config, authService *service.AuthService) auth.OAuthProvider {
	return &OxSciOAuthProvider{
		cfg:         cfg,
		authService: authService,
	}
}

func (p *OxSciOAuthProvider) Name() string {
	return "oxsci"
}

func (p *OxSciOAuthProvider) IsEnabled(ctx context.Context) bool {
	return p.cfg != nil && p.cfg.OxSci.Enabled
}

func (p *OxSciOAuthProvider) StartAuth(c *gin.Context) {
	cfg, err := p.getOxSciOAuthConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// ... 原来的 OxSciOAuthStart 逻辑
}

func (p *OxSciOAuthProvider) HandleCallback(c *gin.Context) {
	// ... 原来的 OxSciOAuthCallback 逻辑
}

// ... 其他辅助方法保持不变
```

#### Step 3.3: 修改路由注册

修改文件：`backend/internal/server/routes/auth.go`

```go
func RegisterAuthRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	oauthRegistry *auth.OAuthProviderRegistry, // [OXSCI] 新增参数
	jwtAuth servermiddleware.JWTAuthMiddleware,
	redisClient *redis.Client,
) {
	rateLimiter := middleware.NewRateLimiter(redisClient)

	auth := v1.Group("/auth")
	{
		auth.POST("/register", h.Auth.Register)
		auth.POST("/login", h.Auth.Login)
		// ... 其他路由

		// [OXSCI] 动态注册 OAuth Provider 路由
		for _, provider := range oauthRegistry.AllProviders() {
			if provider.IsEnabled(context.Background()) {
				providerName := provider.Name()
				auth.GET("/oauth/"+providerName+"/start", provider.StartAuth)
				auth.GET("/oauth/"+providerName+"/callback", provider.HandleCallback)
			}
		}
	}

	// ... 其他路由组
}
```

#### Step 3.4: 更新 Wire 依赖注入

修改文件：`backend/cmd/server/wire.go`

```go
// +build wireinject

func InitializeApp(cfg *config.Config) (*App, error) {
	wire.Build(
		// ... 现有的 providers

		// [OXSCI] OAuth Providers
		auth.NewOAuthProviderRegistry,
		providers.NewOxSciOAuthProvider,

		// ... 其他
	)
	return &App{}, nil
}
```

修改文件：`backend/cmd/server/main.go`

```go
func (a *App) setupRoutes() {
	v1 := a.router.Group("/api/v1")

	routes.RegisterAuthRoutes(
		v1,
		a.handlers,
		a.oauthRegistry, // [OXSCI] 传入 OAuth Provider Registry
		a.jwtAuthMiddleware,
		a.redisClient,
	)

	// ... 其他路由
}
```

#### Step 3.5: 提交重构

```bash
git add backend/internal/auth/
git add backend/internal/server/routes/auth.go
git add backend/cmd/server/

git commit -m "[OXSCI] refactor(auth): 使用 Provider 模式重构 OAuth 功能

- 引入 OAuthProvider 接口，支持插件化 OAuth 提供商
- 将 OxSci OAuth 实现改造为 Provider
- 路由注册改为动态扫描所有启用的 Provider
- 降低路由层的修改，减少与 upstream 的冲突风险

符合开闭原则：
- 对扩展开放：可以添加新的 OAuth Provider（如 Google, GitHub）
- 对修改封闭：routes/auth.go 只需添加 5 行循环代码

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Phase 4: 优化 Env Config (P3 - 中优先级，可选)

**目标：** 使用 HTTP Client 中间件优化 API Key 注入

这个重构是可选的，因为当前实现的侵入性不高。可以根据时间决定是否执行。

#### Step 4.1: 创建 Client 中间件接口

创建文件：`backend/internal/client/middleware.go`

```go
package client

import "github.com/imroc/req/v3"

type RequestMiddleware interface {
	BeforeRequest(req *req.Request) error
}

type ClientMiddlewareChain struct {
	middlewares []RequestMiddleware
}

func NewClientMiddlewareChain() *ClientMiddlewareChain {
	return &ClientMiddlewareChain{
		middlewares: make([]RequestMiddleware, 0),
	}
}

func (c *ClientMiddlewareChain) Use(middleware RequestMiddleware) {
	c.middlewares = append(c.middlewares, middleware)
}

func (c *ClientMiddlewareChain) Apply(req *req.Request) error {
	for _, middleware := range c.middlewares {
		if err := middleware.BeforeRequest(req); err != nil {
			return err
		}
	}
	return nil
}
```

#### Step 4.2: 实现 API Key 中间件

创建文件：`backend/internal/client/middlewares/api_key.go`

```go
package middlewares

import (
	"github.com/Wei-Shaw/sub2api/internal/client"
	"github.com/imroc/req/v3"
)

type APIKeyMiddleware struct {
	apiKey string
}

func NewAPIKeyMiddleware(apiKey string) client.RequestMiddleware {
	return &APIKeyMiddleware{apiKey: apiKey}
}

func (m *APIKeyMiddleware) BeforeRequest(req *req.Request) error {
	if m.apiKey != "" {
		req.SetHeader("X-API-Key", m.apiKey)
	}
	return nil
}
```

#### Step 4.3: 重构 LLMLogClient

修改文件：`backend/internal/client/llm_log_client.go`

```go
import (
	"github.com/Wei-Shaw/sub2api/internal/client/middlewares"
)

type LLMLogClient struct {
	baseURL     string
	agentName   string
	timeout     time.Duration
	middlewares *ClientMiddlewareChain
}

func NewLLMLogClient(cfg config.LLMLoggingConfig) *LLMLogClient {
	chain := NewClientMiddlewareChain()

	// [OXSCI] 如果配置了 API Key，添加中间件
	if cfg.APIKey != "" {
		chain.Use(middlewares.NewAPIKeyMiddleware(cfg.APIKey))
	}

	return &LLMLogClient{
		baseURL:     cfg.URL,
		agentName:   cfg.AgentName,
		timeout:     time.Duration(cfg.TimeoutSeconds) * time.Second,
		middlewares: chain,
	}
}

func (c *LLMLogClient) doRequest(ctx context.Context, method, path string, body interface{}) error {
	client := req.C().SetTimeout(c.timeout)
	r := client.R().SetContext(ctx).SetHeader("Content-Type", "application/json")

	// [OXSCI] 应用中间件
	if err := c.middlewares.Apply(r); err != nil {
		return fmt.Errorf("apply middlewares: %w", err)
	}

	// ... 发送请求
}
```

#### Step 4.4: 提交重构

```bash
git add backend/internal/client/

git commit -m "[OXSCI] refactor(client): 使用中间件模式优化 HTTP Client

- 引入 RequestMiddleware 接口，支持插件化 HTTP 请求处理
- 将 API Key 注入逻辑抽取为 APIKeyMiddleware
- 提高代码复用性，便于添加更多中间件（如重试、日志）

符合开闭原则：
- 对扩展开放：可以添加新的请求中间件
- 对修改封闭：Client 代码保持稳定

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Phase 5: 最终整合和测试

#### Step 5.1: 运行测试

```bash
# 运行单元测试
cd backend
go test ./...

# 运行集成测试
go test -tags=integration ./...
```

#### Step 5.2: 本地验证

```bash
# 启动服务
make run

# 验证各个定制功能
1. OxSci OAuth 登录
2. LLM Logging
3. SUB2API_LOG_LEVEL 环境变量
4. Proxy API Key 支持
```

#### Step 5.3: 更新文档

创建或更新以下文档：

1. `OXSCI_CUSTOM_FEATURES.md` - 定制功能总览
2. `CONTRIBUTING_OXSCI.md` - 如何添加新的定制功能
3. 更新 `README.md` - 添加 OxSci 定制说明

#### Step 5.4: 强制推送到远程

```bash
git checkout oxsci-custom
git push origin oxsci-custom --force-with-lease
```

---

### Phase 6: 建立持续同步流程

#### Step 6.1: 创建同步脚本

创建文件：`scripts/sync-upstream.sh`

```bash
#!/bin/bash
set -e

echo "=== Syncing with upstream ==="

# 1. 同步 main 分支
echo "[1/3] Syncing main branch..."
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease

# 2. Rebase oxsci-custom 分支
echo "[2/3] Rebasing oxsci-custom..."
git checkout oxsci-custom
git rebase main

# 3. 提示推送
echo "[3/3] Done! Please review changes and run:"
echo "  git push origin oxsci-custom --force-with-lease"
```

```bash
chmod +x scripts/sync-upstream.sh
```

#### Step 6.2: 设置定期同步提醒

在项目 README 中添加：

```markdown
## OxSci 定制版本维护

### 同步 upstream 更新

建议每周同步一次 upstream 更新：

\`\`\`bash
./scripts/sync-upstream.sh
\`\`\`

如果遇到冲突，按以下优先级解决：
1. 配置文件冲突：保留 OxSci 定制部分
2. 路由冲突：保留 OAuth Provider 动态注册逻辑
3. 其他冲突：优先采用 upstream 版本，然后叠加定制功能
```

---

## 时间估算

| Phase | 任务 | 预估时间 | 优先级 |
|-------|------|----------|--------|
| 0 | 备份当前状态 | 5 分钟 | P0 |
| 1 | 清理 Commit 历史 | 30 分钟 | P0 |
| 2 | 重构 SUB2API_LOG_LEVEL | 1.5 小时 | P1 |
| 3 | 重构 OxSci OAuth2 | 2.5 小时 | P2 |
| 4 | 优化 Env Config | 1 小时 | P3 (可选) |
| 5 | 测试和文档 | 1 小时 | P0 |
| 6 | 同步流程 | 30 分钟 | P0 |
| **总计** | | **7-8 小时** | |

## 风险评估

| 风险 | 可能性 | 影响 | 缓解措施 |
|------|--------|------|----------|
| Rebase 冲突 | 中 | 中 | 已创建备份分支，可随时回滚 |
| 重构引入 bug | 低 | 高 | 完整的测试覆盖，分阶段提交 |
| Wire 依赖注入失败 | 低 | 中 | 仔细检查依赖关系，运行 `wire gen` |
| 性能下降 | 极低 | 低 | 接口调用开销可忽略，已验证 |

## 成功标准

- ✅ 所有测试通过
- ✅ OxSci OAuth 登录功能正常
- ✅ LLM Logging 功能正常
- ✅ SUB2API_LOG_LEVEL 环境变量生效
- ✅ Commit 历史清晰，易于维护
- ✅ 文档完善，便于未来扩展

## 后续改进

重构完成后，可以考虑：

1. **自动化测试**：为定制功能添加专门的测试套件
2. **配置验证**：在启动时验证 OxSci 配置的完整性
3. **监控集成**：为定制功能添加监控指标
4. **CI/CD 集成**：自动化同步和测试流程
