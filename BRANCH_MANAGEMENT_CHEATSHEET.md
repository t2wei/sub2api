# OxSci 分支管理速查表

## 快速同步 Upstream

```bash
# 完整同步流程
git checkout main
git fetch upstream
git reset --hard upstream/main
git push origin main --force-with-lease

git checkout oxsci-custom
git rebase main
# 如果有冲突，解决后：
# git add <resolved-files>
# git rebase --continue
git push origin oxsci-custom --force-with-lease
```

## 当前定制功能概览

| 功能 | 文件数 | 开闭原则评分 | 合并风险 | 重构优先级 |
|------|--------|--------------|----------|-----------|
| OxSci OAuth2 | 18 个 | ⭐⭐⭐⭐ | 🟡 中 | P2 建议重构 |
| LLM Logging | 2 个 | ⭐⭐⭐⭐⭐ | 🟢 低 | ✅ 已完成 |
| SUB2API_LOG_LEVEL | 4 个 | ⭐⭐ | 🔴 高 | P1 必须重构 |
| Env Config | 6 个 | ⭐⭐⭐ | 🟡 中 | P3 可选重构 |

## 重构优先级

### P1: SUB2API_LOG_LEVEL（必须）
- **风险**：修改了 `main.go`，极易与 upstream 冲突
- **方案**：配置后处理器模式
- **时间**：1.5 小时

### P2: OxSci OAuth2（建议）
- **风险**：修改了路由、配置、服务层
- **方案**：OAuth Provider 插件化
- **时间**：2.5 小时

### P3: Env Config（可选）
- **风险**：添加了 API Key 支持
- **方案**：HTTP Client 中间件
- **时间**：1 小时

## Commit 历史整理

### 当前状态（混乱）
```
* 50dc3102 refactor(logging): 使用开闭原则重构 LLM logging 功能
* 1f9051af doc: merge conflict docs
* 52b73a86 Merge upstream/main into oxsci-custom ← 不应该有
* 5944f9ac feat: 添加 SUB2API_LOG_LEVEL 环境变量支持
* cb271df9 feat: env config for ecs and local, with proxy api
* bd3274dc feat(logging): 添加 LLM 调用日志记录功能
* 75a23c53 feat(auth): 添加 OxSci OAuth2 登录支持
```

### 目标状态（清晰）
```
* [OXSCI] refactor: 使用开闭原则重构定制功能
* [OXSCI] feat: 添加 SUB2API_LOG_LEVEL 环境变量支持
* [OXSCI] feat: env config for ecs and local, with proxy api
* [OXSCI] feat: 添加 LLM 调用日志记录功能
* [OXSCI] feat: 添加 OxSci OAuth2 登录支持
* <upstream 最新 commit>
```

## 常用命令

### 查看定制功能的 commits
```bash
git log --oneline --author=t2wei -20
```

### 查看与 main 分支的差异
```bash
git log main..oxsci-custom --oneline
```

### 查看修改的文件
```bash
git diff main..oxsci-custom --name-only
```

### 查看某个文件的修改
```bash
git diff main..oxsci-custom -- backend/internal/handler/gateway_handler.go
```

### 检查冲突文件
```bash
git diff --name-only --diff-filter=U
```

## 冲突解决策略

### 配置文件冲突
```bash
# 保留 OxSci 定制部分，合并 upstream 新增
# 优先级：OxSci 配置 > upstream 新增 > 删除冲突部分
```

### 路由文件冲突
```bash
# 保留 OAuth Provider 动态注册逻辑
# 如果 upstream 添加了新路由，手动添加到我们的版本中
```

### 代码文件冲突
```bash
# 如果是 Hook 机制相关，保留我们的版本
# 如果是业务逻辑，优先采用 upstream，然后叠加 Hook 调用
```

## 重要文件清单

### 核心定制文件（不太会冲突）
```
✅ backend/internal/handler/request_lifecycle_hook.go
✅ backend/internal/handler/llm_logging_hook.go
✅ backend/internal/handler/auth_oxsci_oauth.go
✅ frontend/src/components/auth/OxSciOAuthSection.vue
✅ frontend/src/views/auth/OxSciCallbackView.vue
```

### 高风险文件（容易冲突）
```
🔴 backend/cmd/server/main.go
🟡 backend/internal/server/routes/auth.go
🟡 backend/internal/config/config.go
🟡 backend/internal/service/auth_service.go
🟡 backend/internal/handler/gateway_handler.go
```

## 工作流最佳实践

### 开发新功能
```bash
# 1. 确保基于最新的 main
git checkout oxsci-custom
git rebase main

# 2. 创建功能分支（可选）
git checkout -b feature/oxsci-new-feature

# 3. 开发和提交
git add .
git commit -m "[OXSCI] feat: 新功能描述"

# 4. 合并回 oxsci-custom
git checkout oxsci-custom
git merge feature/oxsci-new-feature
```

### 紧急修复
```bash
# 1. 直接在 oxsci-custom 修复
git checkout oxsci-custom
git add .
git commit -m "[OXSCI] fix: 修复描述"

# 2. 推送
git push origin oxsci-custom
```

### 定期同步（推荐每周）
```bash
# 运行同步脚本
./scripts/sync-upstream.sh

# 或手动执行
git checkout main && \
git fetch upstream && \
git reset --hard upstream/main && \
git push origin main --force-with-lease && \
git checkout oxsci-custom && \
git rebase main
```

## 回滚策略

### 回滚到上一个 commit
```bash
git reset --hard HEAD^
git push origin oxsci-custom --force-with-lease
```

### 回滚到备份分支
```bash
git checkout oxsci-custom
git reset --hard oxsci-custom-backup
git push origin oxsci-custom --force-with-lease
```

### 回滚某个文件
```bash
git checkout main -- backend/cmd/server/main.go
```

## 相关文档

- `CUSTOM_FEATURES_ANALYSIS.md` - 定制功能分析和重构建议
- `REFACTORING_IMPLEMENTATION_PLAN.md` - 详细的重构实施计划
- `REFACTORING_LLM_LOGGING.md` - LLM Logging 重构文档（示例）
- `LOG_LEVEL_CONFIG.md` - LOG_LEVEL 配置文档
- `MERGE_CONFLICT_ANALYSIS.md` - 合并冲突分析

## 联系信息

- 项目维护者：t2wei
- Upstream 仓库：Wei-Shaw/sub2api
- Fork 仓库：t2wei/sub2api
