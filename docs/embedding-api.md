# Embedding API 端点说明

## 概览

本项目支持两种协议的 Embedding API，均仅支持 **APIKey 类型账号**。

| 协议 | 端点 | 上游 |
|------|------|------|
| Gemini 原生 | `POST /v1beta/models/{model}:embedContent` | `generativelanguage.googleapis.com` |
| Gemini 原生 | `POST /v1beta/models/{model}:batchEmbedContents` | `generativelanguage.googleapis.com` |
| OpenAI 兼容 | `POST /v1/embeddings` | `api.openai.com` |

## Gemini 原生 Embedding

通过已有的 `/v1beta/models/*modelAction` 通配符路由支持，无需新增路由。
`ForwardNative` 的 action 白名单中包含 `embedContent` 和 `batchEmbedContents`。

### embedContent（单条）

```bash
curl -X POST "http://localhost:8080/v1beta/models/text-embedding-004:embedContent" \
  -H "x-goog-api-key: <your-sub2api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "content": {
      "parts": [{"text": "Hello world"}]
    }
  }'
```

响应：
```json
{
  "embedding": {"values": [0.1, 0.2, ...]},
  "usageMetadata": {"promptTokenCount": 5}
}
```

### batchEmbedContents（批量）

```bash
curl -X POST "http://localhost:8080/v1beta/models/text-embedding-004:batchEmbedContents" \
  -H "x-goog-api-key: <your-sub2api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "requests": [
      {"model": "models/text-embedding-004", "content": {"parts": [{"text": "Hello"}]}},
      {"model": "models/text-embedding-004", "content": {"parts": [{"text": "World"}]}}
    ]
  }'
```

响应：
```json
{
  "embeddings": [{"values": [...]}, {"values": [...]}],
  "usageMetadata": {"promptTokenCount": 10}
}
```

### 支持的 Gemini Embedding 模型

- `text-embedding-004`
- `embedding-001`

（更多模型取决于上游 Gemini API Key 的可用范围）

## OpenAI 兼容 Embedding

新增路由 `POST /v1/embeddings`，透传到上游 OpenAI API。
与 `/v1/responses` 模式一致：handler 不关心 platform，账号选择由 service 层完成。

### 请求示例

```bash
# 单条
curl -X POST "http://localhost:8080/v1/embeddings" \
  -H "Authorization: Bearer <your-sub2api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "text-embedding-3-small",
    "input": "Hello world"
  }'

# 批量
curl -X POST "http://localhost:8080/v1/embeddings" \
  -H "Authorization: Bearer <your-sub2api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "text-embedding-3-small",
    "input": ["Hello", "World"]
  }'
```

响应：
```json
{
  "object": "list",
  "data": [
    {"object": "embedding", "index": 0, "embedding": [0.1, 0.2, ...]}
  ],
  "model": "text-embedding-3-small",
  "usage": {"prompt_tokens": 5, "total_tokens": 5}
}
```

## 计费

- **Input Tokens**：Gemini 从 `usageMetadata.promptTokenCount` 获取，OpenAI 从 `usage.prompt_tokens` 获取
- **Output Tokens**：始终为 0
- **长上下文双倍计费**：不适用于 Embedding（仅 generateContent 触发）

## 实现文件

| 文件 | 职责 |
|------|------|
| `internal/server/routes/gateway.go` | 路由注册（`POST /v1/embeddings`） |
| `internal/handler/openai_gateway_handler.go` | OpenAI Embeddings handler |
| `internal/service/openai_gateway_service.go` | `ForwardEmbeddings` — OpenAI 透传 |
| `internal/handler/gemini_v1beta_handler.go` | Gemini 原生 handler（含 embedding 计费分支） |
| `internal/service/gemini_messages_compat_service.go` | `ForwardNative` action 白名单扩展、`extractGeminiEmbeddingUsage` |
| `internal/pkg/gemini/models.go` | Embedding 模型 fallback 列表 |

## 扩展指南

### 新增其他平台的 Embedding 支持

1. 在对应 platform 的 service 中实现 `ForwardEmbeddings` 方法
2. 如果是新协议（非 OpenAI/Gemini），需要新增路由
3. 如果是已有协议下的新 action，只需扩展对应的 action 白名单
