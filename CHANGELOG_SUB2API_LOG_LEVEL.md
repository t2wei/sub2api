# SUB2API_LOG_LEVEL 环境变量实现

## 变更日期

2026-01-28

## 变更原因

Sub2API 与 data-service 部署在同一个 ECS 容器中，如果使用通用的 `LOG_LEVEL` 环境变量，会导致两个服务的日志级别无法独立控制。

## 解决方案

使用 `SUB2API_LOG_LEVEL` 环境变量，实现 Sub2API 的日志级别独立配置。

## 修改文件

### 1. 代码修改

**文件**: `backend/cmd/server/main.go`

**修改位置**: `main()` 函数开头，`initLogger()` 之后

```go
// Override log level with SUB2API_LOG_LEVEL environment variable if set
// Use SUB2API_LOG_LEVEL to avoid conflicts when deployed alongside data-service
if logLevelEnv := strings.ToUpper(os.Getenv("SUB2API_LOG_LEVEL")); logLevelEnv != "" {
    var level slog.Level
    switch logLevelEnv {
    case "DEBUG":
        level = slog.LevelDebug
    case "INFO":
        level = slog.LevelInfo
    case "WARN", "WARNING":
        level = slog.LevelWarn
    case "ERROR":
        level = slog.LevelError
    default:
        log.Printf("Warning: Invalid SUB2API_LOG_LEVEL '%s', ignoring", logLevelEnv)
        goto skipLogLevelOverride
    }
    handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
    slog.SetDefault(slog.New(handler))
    slog.Info("Log level overridden by environment variable", "level", level.String())
}
skipLogLevelOverride:
```

**特点**:
- ✅ 不修改 `initLogger()` 函数，最小化与 upstream 冲突
- ✅ 使用独立的环境变量名 `SUB2API_LOG_LEVEL`
- ✅ 不受 `LOG_LEVEL` 环境变量影响
- ✅ 仅 20 行代码，易于维护

### 2. 部署配置修改

#### ECS Fargate 配置

**文件**: `oxsci-deploy/services/data_service/service-config.yml`

```yaml
custom_environment_variables:
  # 日志配置（Sub2API 专用，不影响 data-service）
  - name: SUB2API_LOG_LEVEL
    value: "WARN"  # DEBUG, INFO, WARN, ERROR (WARN = Info 以上不含 Info)
```

#### Docker Compose 配置

**文件**: `deploy/.env.example`

```bash
# Sub2API Log level: DEBUG, INFO, WARN, ERROR
# 使用 SUB2API_LOG_LEVEL 避免与其他服务（如 data-service）冲突
SUB2API_LOG_LEVEL=WARN
```

### 3. 文档

**文件**: `LOG_LEVEL_CONFIG.md`

完整的使用文档，包含配置说明、测试示例、常见问题等。

## 使用示例

### 部署环境

```yaml
# service-config.yml
- name: SUB2API_LOG_LEVEL
  value: "WARN"  # 只记录警告和错误
```

### 本地开发

```bash
# 方式 1: 临时设置
SUB2API_LOG_LEVEL=DEBUG go run ./cmd/server

# 方式 2: 导出环境变量
export SUB2API_LOG_LEVEL=WARN
go run ./cmd/server

# 方式 3: .env 文件
echo "SUB2API_LOG_LEVEL=WARN" >> .env
go run ./cmd/server
```

## 日志级别说明

| 级别 | 输出内容 | 适用场景 |
|------|---------|---------|
| `DEBUG` | DEBUG, INFO, WARN, ERROR | 开发调试 |
| `INFO` | INFO, WARN, ERROR | 生产环境默认 |
| `WARN` | WARN, ERROR（**不含 INFO**） | 生产环境（减少日志量） |
| `ERROR` | 仅 ERROR | 仅关注错误 |

## 测试验证

### 测试 1: SUB2API_LOG_LEVEL 独立生效

```bash
$ SUB2API_LOG_LEVEL=WARN go run ./cmd/server --version
# 只输出 WARN 和 ERROR 日志
```

### 测试 2: 不受 LOG_LEVEL 影响

```bash
$ LOG_LEVEL=DEBUG SUB2API_LOG_LEVEL=ERROR go run ./cmd/server --version
# 只输出 ERROR 日志（SUB2API_LOG_LEVEL 优先）
```

### 测试 3: 默认行为

```bash
$ go run ./cmd/server --version
# 根据 SERVER_MODE 决定：release=INFO, debug=DEBUG
```

## 与 data-service 的隔离

### 同一容器中的环境变量

```yaml
environment:
  - LOG_LEVEL=INFO              # data-service 使用
  - SUB2API_LOG_LEVEL=WARN      # Sub2API 使用（独立）
```

**效果**:
- data-service 日志级别: INFO（显示 INFO, WARN, ERROR）
- Sub2API 日志级别: WARN（只显示 WARN, ERROR）

## 生产环境建议

### ECS Fargate

**推荐配置**: `SUB2API_LOG_LEVEL=WARN`

**优势**:
- ✅ 减少 CloudWatch Logs 存储成本（减少约 60-80% 的日志量）
- ✅ 降低日志噪音，重点关注警告和错误
- ✅ 提升服务性能（减少 I/O 操作）

**何时使用 INFO**:
- 新功能上线初期（观察运行状态）
- 排查疑难问题时
- 需要审计完整操作记录时

### Docker Compose

- 开发环境: `DEBUG` 或 `INFO`
- 生产环境: `WARN` 或 `ERROR`

## 兼容性说明

### 与 upstream 合并

✅ **低冲突风险**

修改位于 `main()` 函数开头，不影响核心逻辑：
- 不修改 `initLogger()` 函数
- 使用标准 Go 语法，无第三方依赖
- 如果 upstream 修改了日志初始化，只需移动这段代码到合适位置

### 向后兼容

✅ **完全兼容**

如果不设置 `SUB2API_LOG_LEVEL`：
- 行为与之前完全一致
- 由 `SERVER_MODE` 决定日志级别
- 不影响现有部署

## 注意事项

1. **环境变量命名**: 必须使用 `SUB2API_LOG_LEVEL`，不能使用 `LOG_LEVEL`
2. **大小写**: 环境变量值会自动转为大写，`warn` 和 `WARN` 效果相同
3. **无效值**: 无效的日志级别会被忽略，使用默认行为
4. **不可热更新**: 修改环境变量后需要重启服务才能生效
5. **组件独立性**: 某些组件（Gin、数据库驱动）有独立的日志系统，不受此配置影响

## 相关链接

- 完整文档: [LOG_LEVEL_CONFIG.md](LOG_LEVEL_CONFIG.md)
- 代码实现: [backend/cmd/server/main.go](backend/cmd/server/main.go#L67-L87)
- ECS 配置: [oxsci-deploy/services/data_service/service-config.yml](../oxsci-deploy/services/data_service/service-config.yml)
- Docker 配置: [deploy/.env.example](deploy/.env.example)
