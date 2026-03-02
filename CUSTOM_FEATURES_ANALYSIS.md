# OxSci 定制功能分析与重构建议

## 当前分支管理策略问题

### 问题描述

当前 `oxsci-custom` 分支的 commit 历史：

```
* 50dc3102 refactor(logging): 使用开闭原则重构 LLM logging 功能  ← 定制 #4 (重构)
* 1f9051af doc: merge conflict docs                              ← 定制 #3 (文档)
* 52b73a86 Merge upstream/main into oxsci-custom                 ← Merge commit
* 5944f9ac feat: 添加 SUB2API_LOG_LEVEL 环境变量支持             ← 定制 #2
* cb271df9 feat: env config for ecs and local, with proxy api     ← 定制 #1.5
* bd3274dc feat(logging): 添加 LLM 调用日志记录功能             ← 定制 #1 (已重构)
* 75a23c53 feat(auth): 添加 OxSci OAuth2 登录支持               ← 定制 #0
```

**问题：**
- 定制功能的 commit 与 upstream merge commit 混在一起
- 每次 merge upstream 会产生新的 merge commit
- 无法清晰看到定制功能的演进历史
- 容易产生合并冲突（因为直接修改了核心代码）

### 解决方案：使用 Rebase 工作流

**目标：** 保持 `oxsci-custom` 分支始终基于最新的 `main` 分支，定制功能的 commit 永远在最上面。

**步骤：**

```bash
# 1. 确保 main 分支与 upstream 同步
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease

# 2. 将 oxsci-custom 分支 rebase 到最新的 main 上
git checkout oxsci-custom
git rebase main

# 3. 如果有冲突，解决冲突后继续
# git add <resolved-files>
# git rebase --continue

# 4. 强制推送（因为改变了历史）
git push origin oxsci-custom --force-with-lease
```

**预期结果：**

```
* 定制功能 commit #4
* 定制功能 commit #3
* 定制功能 commit #2
* 定制功能 commit #1
* 定制功能 commit #0
* <upstream 最新 commit>
* <upstream 历史>
```

**优势：**
- ✅ 清晰的 commit 历史
- ✅ 定制功能始终在最新的 upstream 上
- ✅ 容易追踪定制功能的演进
- ✅ 容易回滚某个定制功能（直接 revert 一个 commit）

---

## 定制功能分类与开闭原则分析

### 1. ✅ LLM Logging 功能 - **已重构，符合开闭原则**

**实现方式：** Hook 机制（`RequestLifecycleHook`）

**文件变更：**
- ✅ 新增：`backend/internal/handler/request_lifecycle_hook.go`
- ✅ 新增：`backend/internal/handler/llm_logging_hook.go`
- ⚠️ 修改：`backend/internal/handler/gateway_handler.go`（引入 hook 字段）

**开闭原则评分：** ⭐⭐⭐⭐⭐ (5/5)

**特点：**
- 对扩展开放：新增 Hook 实现即可
- 对修改封闭：核心业务逻辑不需要修改
- 合并友好：新增文件，修改部分最小化

**未来升级策略：**
- Upstream 更新不太可能影响 Hook 机制
- 如果 `gateway_handler.go` 有冲突，只需要保留 `lifecycleHook` 字段即可

---

### 2. ⚠️ OxSci OAuth2 登录 - **部分符合开闭原则，需要优化**

**实现方式：** 新增 handler + 路由注册

**文件变更：**
- ✅ 新增：`backend/internal/handler/auth_oxsci_oauth.go`（646 行，独立文件）
- ✅ 新增：`frontend/src/components/auth/OxSciOAuthSection.vue`
- ✅ 新增：`frontend/src/views/auth/OxSciCallbackView.vue`
- ⚠️ 修改：`backend/internal/server/routes/auth.go`（+2 行路由）
- ⚠️ 修改：`backend/internal/config/config.go`（+79 行配置）
- ⚠️ 修改：`backend/internal/service/auth_service.go`（+18 行）
- ⚠️ 修改：多个前端 i18n 文件

**开闭原则评分：** ⭐⭐⭐⭐ (4/5)

**冲突风险点：**
1. **路由注册**（`routes/auth.go`）
   - 风险：中等
   - 原因：如果 upstream 添加新的 OAuth 路由，可能冲突

2. **配置结构**（`config/config.go`）
   - 风险：中等
   - 原因：如果 upstream 修改配置结构，可能冲突

3. **认证服务**（`auth_service.go`）
   - 风险：中等
   - 原因：添加了 `LoginOrRegisterOAuthTrusted` 方法

**重构建议：使用插件化 OAuth Provider**

#### 当前实现：
```go
// routes/auth.go
auth.GET("/oauth/oxsci/start", h.Auth.OxSciOAuthStart)
auth.GET("/oauth/oxsci/callback", h.Auth.OxSciOAuthCallback)
```

#### 建议重构：

**步骤 1：定义 OAuth Provider 接口**

新增文件：`backend/internal/auth/oauth_provider.go`

```go
package auth

import (
	"context"
	"github.com/gin-gonic/gin"
)

// OAuthProvider 定义 OAuth 提供商接口
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

**步骤 2：将 OxSci OAuth 改造为 Provider**

新增文件：`backend/internal/auth/providers/oxsci_provider.go`

```go
package providers

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/auth"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
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
	// 将 auth_oxsci_oauth.go 中的 OxSciOAuthStart 逻辑移到这里
	// ...
}

func (p *OxSciOAuthProvider) HandleCallback(c *gin.Context) {
	// 将 auth_oxsci_oauth.go 中的 OxSciOAuthCallback 逻辑移到这里
	// ...
}
```

**步骤 3：统一路由注册**

修改：`backend/internal/server/routes/auth.go`

```go
func RegisterAuthRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	oauthRegistry *auth.OAuthProviderRegistry, // 新增参数
	jwtAuth servermiddleware.JWTAuthMiddleware,
	redisClient *redis.Client,
) {
	// ... 其他路由

	// 动态注册所有 OAuth Provider 的路由
	auth := v1.Group("/auth")
	for _, provider := range oauthRegistry.AllProviders() {
		if provider.IsEnabled(context.Background()) {
			providerName := provider.Name()
			auth.GET("/oauth/"+providerName+"/start", provider.StartAuth)
			auth.GET("/oauth/"+providerName+"/callback", provider.HandleCallback)
		}
	}
}
```

**步骤 4：Wire 依赖注入**

修改：`backend/cmd/server/wire.go`

```go
// +build wireinject

func InitializeApp(cfg *config.Config) (*App, error) {
	wire.Build(
		// ... 现有的 providers

		// OAuth Providers
		auth.NewOAuthProviderRegistry,
		providers.NewOxSciOAuthProvider,
		wire.Bind(new(auth.OAuthProvider), new(*providers.OxSciOAuthProvider)),

		// ...
	)
	return &App{}, nil
}
```

**重构后的优势：**

1. ✅ **对扩展开放**：添加新的 OAuth Provider 只需实现接口
2. ✅ **对修改封闭**：不需要修改路由注册代码
3. ✅ **合并友好**：即使 upstream 修改 `routes/auth.go`，我们的改动最小化
4. ✅ **插件化架构**：可以轻松启用/禁用 Provider

---

### 3. ⚠️ SUB2API_LOG_LEVEL 环境变量 - **侵入性修改，需要优化**

**实现方式：** 直接修改 `main.go`

**文件变更：**
- ⚠️ 修改：`backend/cmd/server/main.go`（+23 行）
- ✅ 新增：`LOG_LEVEL_CONFIG.md`（文档）
- ✅ 新增：`CHANGELOG_SUB2API_LOG_LEVEL.md`（文档）

**开闭原则评分：** ⭐⭐ (2/5)

**冲突风险点：**
- **`main.go`**：非常高风险
- 原因：`main.go` 是应用入口，upstream 修改频率高

**重构建议：使用配置加载 Hook**

#### 当前实现：
```go
// main.go
func main() {
	// ... 加载配置
	cfg, err := config.LoadConfig()

	// 硬编码的环境变量覆盖逻辑
	if logLevel := os.Getenv("SUB2API_LOG_LEVEL"); logLevel != "" {
		// ... 23 行逻辑
	}

	// ... 启动应用
}
```

#### 建议重构：配置后处理器（Post-Processor）

**步骤 1：定义配置后处理器接口**

新增文件：`backend/internal/config/post_processor.go`

```go
package config

import "context"

// ConfigPostProcessor 配置加载后的处理器
type ConfigPostProcessor interface {
	// Process 处理配置，返回修改后的配置
	Process(ctx context.Context, cfg *Config) (*Config, error)

	// Priority 返回处理优先级（数字越小越先执行）
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
	// 按优先级排序
	sort.Slice(c.processors, func(i, j int) bool {
		return c.processors[i].Priority() < c.processors[j].Priority()
	})
}

func (c *ConfigPostProcessorChain) Process(ctx context.Context, cfg *Config) (*Config, error) {
	for _, processor := range c.processors {
		var err error
		cfg, err = processor.Process(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}
	return cfg, nil
}
```

**步骤 2：实现日志级别环境变量处理器**

新增文件：`backend/internal/config/processors/log_level_env_processor.go`

```go
package processors

import (
	"context"
	"os"
	"strings"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

type LogLevelEnvProcessor struct{}

func NewLogLevelEnvProcessor() config.ConfigPostProcessor {
	return &LogLevelEnvProcessor{}
}

func (p *LogLevelEnvProcessor) Priority() int {
	return 10 // 优先级 10（较早执行）
}

func (p *LogLevelEnvProcessor) Process(ctx context.Context, cfg *config.Config) (*config.Config, error) {
	logLevel := os.Getenv("SUB2API_LOG_LEVEL")
	if logLevel == "" {
		return cfg, nil
	}

	// 将原来 main.go 中的逻辑移到这里
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
		return cfg, nil
	}

	cfg.Log.Level = zapLevel
	return cfg, nil
}
```

**步骤 3：在 main.go 中使用后处理器链**

修改：`backend/cmd/server/main.go`

```go
func main() {
	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 应用配置后处理器（定制功能的扩展点）
	postProcessorChain := config.NewConfigPostProcessorChain()
	postProcessorChain.Register(processors.NewLogLevelEnvProcessor())
	// 未来可以继续添加其他处理器：
	// postProcessorChain.Register(processors.NewOxSciEnvProcessor())

	cfg, err = postProcessorChain.Process(context.Background(), cfg)
	if err != nil {
		log.Fatalf("Failed to process config: %v", err)
	}

	// ... 启动应用
}
```

**重构后的优势：**

1. ✅ **对扩展开放**：添加新的环境变量处理只需实现新的 Processor
2. ✅ **对修改封闭**：`main.go` 只有 3 行改动，非常稳定
3. ✅ **合并友好**：即使 upstream 修改 `main.go`，冲突风险极低
4. ✅ **测试友好**：Processor 可以单独测试

---

### 4. ⚠️ Env Config (ECS/Local/Proxy) - **配置文件变更，风险较低**

**实现方式：** 修改配置示例和 client 代码

**文件变更：**
- ⚠️ 修改：`backend/internal/client/llm_log_client.go`（+10 行）
- ⚠️ 修改：`backend/internal/config/config.go`（+21 行）
- ⚠️ 修改：`backend/internal/handler/auth_oxsci_oauth.go`（+17 行，API Key 支持）
- ✅ 新增：`deploy/.env.example`（+48 行）
- ⚠️ 修改：`deploy/config.example.yaml`（+58 行）

**开闭原则评分：** ⭐⭐⭐ (3/5)

**冲突风险点：**
1. **config.go**：中等风险（添加了配置字段）
2. **client 代码**：低风险（添加了 API Key header）
3. **配置示例**：低风险（示例文件通常不会冲突）

**分析：**

这个改动主要是添加环境配置支持（ECS vs Local），以及为请求添加 API Key header。

**是否可以使用开闭原则重构？**

可以考虑使用 **HTTP Client Interceptor** 模式：

#### 建议重构：HTTP Client 中间件

**步骤 1：定义 HTTP Client 中间件接口**

新增文件：`backend/internal/client/middleware.go`

```go
package client

import "github.com/imroc/req/v3"

// RequestMiddleware HTTP 请求中间件
type RequestMiddleware interface {
	// BeforeRequest 在请求发送前执行
	BeforeRequest(req *req.Request) error
}

// ClientMiddlewareChain 中间件链
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

**步骤 2：实现 API Key 中间件**

新增文件：`backend/internal/client/middlewares/api_key_middleware.go`

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

**步骤 3：在 Client 中使用中间件**

修改：`backend/internal/client/llm_log_client.go`

```go
type LLMLogClient struct {
	baseURL     string
	agentName   string
	timeout     time.Duration
	middlewares *ClientMiddlewareChain
}

func NewLLMLogClient(cfg config.LLMLoggingConfig) *LLMLogClient {
	middlewares := NewClientMiddlewareChain()

	// 如果配置了 API Key，添加 API Key 中间件
	if cfg.APIKey != "" {
		middlewares.Use(middlewares2.NewAPIKeyMiddleware(cfg.APIKey))
	}

	return &LLMLogClient{
		baseURL:     cfg.URL,
		agentName:   cfg.AgentName,
		timeout:     cfg.TimeoutSeconds,
		middlewares: middlewares,
	}
}

func (c *LLMLogClient) doRequest(method, path string, body interface{}) error {
	req := client.R().SetContext(ctx)

	// 应用中间件
	if err := c.middlewares.Apply(req); err != nil {
		return err
	}

	// ... 发送请求
}
```

**优势：**
- ✅ 扩展性强：可以轻松添加新的请求中间件（如日志、重试、熔断）
- ✅ 复用性高：中间件可以在多个 Client 之间复用

---

## 整体重构优先级

### 优先级 1：高风险 - SUB2API_LOG_LEVEL（必须重构）

- **理由**：修改了 `main.go`，与 upstream 冲突风险极高
- **建议**：使用配置后处理器（Post-Processor）模式
- **收益**：大幅降低合并冲突风险

### 优先级 2：中风险 - OxSci OAuth2（建议重构）

- **理由**：修改了多个核心文件（routes, config, service）
- **建议**：使用 OAuth Provider 插件化架构
- **收益**：降低路由和服务层的冲突风险

### 优先级 3：低风险 - Env Config（可选重构）

- **理由**：主要是配置文件和小的改动
- **建议**：使用 HTTP Client 中间件（可选）
- **收益**：提高代码复用性

### 优先级 4：已完成 - LLM Logging（无需重构）

- **理由**：已经使用了 Hook 模式，符合开闭原则
- **建议**：保持现状
- **收益**：合并友好

---

## 分支管理最佳实践

### 推荐工作流

```bash
# 定期同步 upstream
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease

# Rebase oxsci-custom 分支
git checkout oxsci-custom
git rebase main

# 如果有冲突，解决后继续
git add .
git rebase --continue

# 强制推送
git push origin oxsci-custom --force-with-lease
```

### Commit 组织建议

建议将当前的 commits 重新组织为更清晰的结构：

```
* [OXSCI] refactor: 使用开闭原则重构定制功能
  - 重构 LLM Logging 为 Hook 模式
  - 重构 SUB2API_LOG_LEVEL 为配置后处理器
  - 重构 OxSci OAuth 为 Provider 插件

* [OXSCI] feat: 添加 OxSci OAuth2 登录支持
* [OXSCI] feat: 添加环境配置和 Proxy API 支持
* [OXSCI] feat: 添加 SUB2API_LOG_LEVEL 环境变量支持
* [OXSCI] feat: 添加 LLM 调用日志记录功能
```

使用 `[OXSCI]` 前缀可以快速识别定制功能的 commit。

---

## 总结

| 功能 | 当前评分 | 重构后评分 | 优先级 | 合并风险 |
|------|----------|-----------|--------|----------|
| LLM Logging | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ | - | 🟢 低 |
| OxSci OAuth2 | ⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ | P2 | 🟡 中 |
| SUB2API_LOG_LEVEL | ⭐⭐ | ⭐⭐⭐⭐⭐ | P1 | 🔴 高 |
| Env Config | ⭐⭐⭐ | ⭐⭐⭐⭐ | P3 | 🟡 中 |

**核心原则：最小化对 upstream 核心代码的修改，最大化使用插件化扩展。**
