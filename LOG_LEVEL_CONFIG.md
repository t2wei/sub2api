# SUB2API_LOG_LEVEL 环境变量配置

## 概述

Sub2API 支持通过 `SUB2API_LOG_LEVEL` 环境变量动态设置日志级别，无需修改代码或配置文件。

**为什么使用 `SUB2API_LOG_LEVEL` 而非 `LOG_LEVEL`？**

- Sub2API 可能与 data-service 部署在同一个 ECS 容器中
- 使用 `SUB2API_LOG_LEVEL` 避免与其他服务的日志配置冲突
- 实现服务间的日志级别隔离

## 实现方式

**设计原则**: 最小化代码侵入，便于与 upstream 合并

实现位置: [backend/cmd/server/main.go](backend/cmd/server/main.go)

在 `main()` 函数开头，`initLogger()` 之后添加了环境变量覆盖逻辑：

```go
func main() {
    // Initialize slog logger based on gin mode
    initLogger()

    // Override log level with SUB2API_LOG_LEVEL environment variable if set
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

    // ... 后续代码
}
```

**特点**:
- ✅ 不修改 `initLogger()` 函数，避免与 upstream 冲突
- ✅ 环境变量优先级高于 `SERVER_MODE`
- ✅ 无效值会回退到默认行为
- ✅ 支持 `WARN` 和 `WARNING` 两种写法

## 支持的日志级别

| 级别 | 说明 | 输出内容 | 适用场景 |
|------|------|---------|---------|
| `DEBUG` | 调试级别 | 所有日志（DEBUG, INFO, WARN, ERROR） | 开发调试 |
| `INFO` | 信息级别 | INFO, WARN, ERROR | 生产环境默认 |
| `WARN` | 警告级别 | WARN, ERROR（**不含 INFO**） | 生产环境（减少日志量） |
| `ERROR` | 错误级别 | 仅 ERROR | 仅关注错误 |

**注意**: 如果设置 `SUB2API_LOG_LEVEL=WARN`，将**只显示 WARN 和 ERROR 级别的日志**，INFO 日志会被过滤。

## 使用方法

### 1. Docker Compose 部署

编辑 [deploy/.env](deploy/.env) 文件：

```bash
# 日志级别: DEBUG, INFO, WARN, ERROR
# WARN = Info 以上不含 Info（只显示 WARN 和 ERROR）
SUB2API_LOG_LEVEL=WARN
```

启动服务：
```bash
cd deploy
docker-compose up -d
```

### 2. ECS Fargate 部署

已在 [oxsci-deploy/services/data_service/service-config.yml](../oxsci-deploy/services/data_service/service-config.yml) 中配置：

```yaml
custom_environment_variables:
  # 日志配置
  - name: SUB2API_LOG_LEVEL
    value: "WARN"  # DEBUG, INFO, WARN, ERROR (WARN = Info 以上不含 Info)
```

修改后重新部署：
```bash
cd /Users/t2wei/git/oxsci-deploy
./deploy.sh data-service dev
```

### 3. 本地开发

#### 方式 A: 环境变量
```bash
export SUB2API_LOG_LEVEL=WARN
go run ./cmd/server
```

#### 方式 B: 临时设置
```bash
SUB2API_LOG_LEVEL=DEBUG go run ./cmd/server
```

#### 方式 C: .env 文件
```bash
echo "SUB2API_LOG_LEVEL=WARN" >> .env
go run ./cmd/server
```

### 4. 直接运行二进制

```bash
SUB2API_LOG_LEVEL=WARN ./sub2api
```

## 默认行为

如果 **不设置** `SUB2API_LOG_LEVEL` 环境变量，日志级别由 `SERVER_MODE` 决定：

| SERVER_MODE | 默认日志级别 |
|-------------|-------------|
| `release` | `INFO` |
| `debug` | `DEBUG` |

## 验证配置

启动服务后，查看日志确认级别：

```bash
# Docker
docker-compose logs | grep "Log level"

# 本地
SUB2API_LOG_LEVEL=WARN go run ./cmd/server 2>&1 | grep "Log level"
```

输出示例：
```
time=2026-01-28T20:04:49.868+08:00 level=INFO msg="Log level overridden by environment variable" level=WARN
```

## 测试示例

### 测试脚本

```bash
#!/bin/bash

echo "=== 测试不同日志级别 ==="

echo "1. DEBUG 级别（显示所有日志）"
SUB2API_LOG_LEVEL=DEBUG ./sub2api --version

echo "2. INFO 级别（不显示 DEBUG）"
SUB2API_LOG_LEVEL=INFO ./sub2api --version

echo "3. WARN 级别（不显示 DEBUG 和 INFO）"
SUB2API_LOG_LEVEL=WARN ./sub2api --version

echo "4. ERROR 级别（仅显示错误）"
SUB2API_LOG_LEVEL=ERROR ./sub2api --version
```

### 预期输出

**SUB2API_LOG_LEVEL=DEBUG**:
```
time=... level=DEBUG msg="Database query" sql="SELECT ..."
time=... level=INFO msg="Request processed" path="/api/v1/users"
time=... level=WARN msg="Rate limit approaching" user_id=123
time=... level=ERROR msg="Database connection failed" error="..."
```

**SUB2API_LOG_LEVEL=INFO**:
```
time=... level=INFO msg="Request processed" path="/api/v1/users"
time=... level=WARN msg="Rate limit approaching" user_id=123
time=... level=ERROR msg="Database connection failed" error="..."
```

**SUB2API_LOG_LEVEL=WARN** (**你的需求：Info 以上不含 Info**):
```
time=... level=WARN msg="Rate limit approaching" user_id=123
time=... level=ERROR msg="Database connection failed" error="..."
```

**SUB2API_LOG_LEVEL=ERROR**:
```
time=... level=ERROR msg="Database connection failed" error="..."
```

## 常见问题

### Q1: 设置 SUB2API_LOG_LEVEL=WARN 后仍然看到 INFO 日志？

A: 检查以下几点：
1. 确认环境变量已正确设置：`echo $SUB2API_LOG_LEVEL`
2. 某些组件（如 Gin、PostgreSQL driver）有自己的日志系统，不受 slog 控制
3. 重启服务以应用新配置

### Q2: 如何完全禁用日志？

A: 不建议完全禁用日志，但可以设置 `SUB2API_LOG_LEVEL=ERROR` 仅显示错误日志。

### Q3: 可以动态修改日志级别吗（不重启）？

A: 当前实现不支持。日志级别在服务启动时读取环境变量并固定。如需修改，需要重启服务。

### Q4: 与 upstream 合并时会冲突吗？

A: 设计时考虑了这个问题：
- 只在 `main()` 开头添加了少量代码
- 不修改 `initLogger()` 函数
- 使用标准 Go 语法，无第三方依赖
- 如果 upstream 修改了日志初始化逻辑，只需将环境变量覆盖代码移到新位置即可

## 生产环境建议

### ECS Fargate 部署

建议设置 `SUB2API_LOG_LEVEL=WARN` 以减少日志量：

**优点**:
- ✅ 减少 CloudWatch Logs 存储成本
- ✅ 减少日志噪音，重点关注警告和错误
- ✅ 提升服务性能（减少 I/O 操作）

**何时使用 INFO**:
- 新功能上线初期（观察运行状态）
- 排查疑难问题
- 审计需求（记录所有操作）

### Docker Compose 部署

本地开发建议使用 `DEBUG` 或 `INFO`，生产环境使用 `WARN` 或 `ERROR`。

## 相关文件

- 代码实现: [backend/cmd/server/main.go](backend/cmd/server/main.go)
- Docker 配置: [deploy/.env.example](deploy/.env.example)
- ECS 配置: [oxsci-deploy/services/data_service/service-config.yml](../oxsci-deploy/services/data_service/service-config.yml)
- 本文档: [LOG_LEVEL_CONFIG.md](LOG_LEVEL_CONFIG.md)

## 更新日志

- **2026-01-28**: 初始实现
  - 支持 DEBUG, INFO, WARN, ERROR 四个级别
  - 通过 SUB2API_LOG_LEVEL 环境变量配置
  - 最小化代码修改，便于 upstream 合并
