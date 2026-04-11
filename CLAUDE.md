# CLAUDE.md - gollmproxy 開発ガイド

## ビルド・実行

```bash
go build -o gollmproxy .
./gollmproxy
```

テスト用の起動:
```bash
GEMINI_API_KEY="..." ./gollmproxy -port 9090
```

## プロジェクト構成

全ファイルが `package main` のフラット構成。Go 1.22+ の `http.ServeMux` パターンマッチングを使用。

```
main.go                       エントリーポイント (設定読込 → ロガー初期化 → サーバー起動)
config.go                     Config構造体, 環境変数/YAML/フラグ読込
server.go                     HTTPサーバー, ルーティング登録, ミドルウェア (recovery, requestID, logging)
log.go                        JSONL リクエストロガー (RequestLogger, LogEntry)
proxy.go                      汎用HTTP転送 (forwardRequest), エラーレスポンスヘルパー
openai_compat.go              POST /v1/chat/completions ハンドラ (プロバイダ振り分け, OpenAI/Gemini/Ollama処理)
bedrock.go                    Bedrock Runtime ハンドラ (OpenAI互換リクエストを InvokeModel に転送)
passthrough.go                /openai/*, /gemini/*, /tavily/* パススルーハンドラ
convert_openai_to_gemini.go   OpenAI → Gemini リクエスト変換
convert_gemini_to_openai.go   Gemini → OpenAI レスポンス変換 (非ストリーミング/ストリーミング)
types_openai.go               OpenAI API 型定義 (Request, Response, StreamChunk, Error)
types_gemini.go               Gemini API 型定義 (Request, Response, UsageMetadata)
stream.go                     SSE ストリーミングユーティリティ (proxySSEStream)
config.yaml.example           設定ファイル例
```

## アーキテクチャ

### リクエストフロー

```
クライアント
  │
  ├─ POST /v1/chat/completions ──→ openai_compat.go
  │     model: "gemini/gemini-2.5-flash"
  │       │
  │       ├─ provider=openai → OpenAI APIにそのまま転送 (modelフィールドのプレフィックス除去のみ)
  │       ├─ provider=gemini → convert_openai_to_gemini.go で変換 → Gemini API
  │       │                     → convert_gemini_to_openai.go でレスポンス変換
  │       ├─ provider=ollama_chat → OpenAI互換APIとして転送 (APIキー不要, デフォルト: localhost:11434)
  │       └─ provider=bedrock → AWS Bedrock Runtime InvokeModel に転送
  │
  ├─ /openai/* ──→ passthrough.go → api.openai.com (Authorization: Bearer)
  ├─ /gemini/* ──→ passthrough.go → generativelanguage.googleapis.com (?key=)
  └─ /tavily/* ──→ passthrough.go → api.tavily.com (Authorization: Bearer)
```

### ミドルウェアチェーン

```
recoveryMiddleware → requestIDMiddleware → loggingMiddleware → [authMiddleware] → ルーターハンドラ
```

- **recovery**: パニック捕捉、500レスポンス
- **requestID**: UUID生成、X-Request-IDヘッダ、コンテキスト注入
- **logging**: ステータスコード・レイテンシ記録
- **auth** (master_key設定時のみ): APIキー認証（/health はスキップ）

### ストリーミング

- OpenAI→OpenAI: SSEレスポンスをそのままパイプスルー
- Gemini→OpenAI: Gemini SSEチャンクを1つずつ読み取り、OpenAI SSEフォーマット(delta形式)に変換
- `stream.go` の `proxySSEStream` が共通処理。変換関数を渡すことで対応

### OpenAI ↔ Gemini 変換マッピング

| OpenAI | Gemini |
|--------|--------|
| messages[role=system] | systemInstruction |
| messages[role=user] | contents[role=user] |
| messages[role=assistant] | contents[role=model] |
| max_tokens | generationConfig.maxOutputTokens |
| temperature | generationConfig.temperature |
| top_p | generationConfig.topP |
| stop | generationConfig.stopSequences |
| finish_reason: stop | finishReason: STOP |
| finish_reason: length | finishReason: MAX_TOKENS |
| finish_reason: content_filter | finishReason: SAFETY |

### APIキー注入方式

| プロバイダ | 方式 |
|-----------|------|
| OpenAI | `Authorization: Bearer $OPENAI_API_KEY` ヘッダ |
| Gemini | `?key=$GEMINI_API_KEY` クエリパラメータ |
| Tavily | `Authorization: Bearer $TAVILY_API_KEY` ヘッダ |
| Ollama Chat | APIキー不要 (設定時は `Authorization: Bearer` ヘッダ) |
| Bedrock | AWS SDK の標準認証情報チェーン + `region` |

## 設定ファイル形式

```yaml
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  litellm_key_header_name: X-Litellm-Api-Key
  port: 8080
  log_file: "gollmproxy.log"

model_list:
  - model_name: gpt-4o
    litellm_params:
      model: openai/gpt-4o
      api_key: os.environ/OPENAI_API_KEY
      api_base: https://api.openai.com
  - model_name: gemini-flash
    litellm_params:
      model: gemini/gemini-2.5-flash
      api_key: os.environ/GEMINI_API_KEY
  - model_name: gpt-oss-20b
    litellm_params:
      model: bedrock/openai.gpt-oss-20b-1:0
      region: ap-northeast-1

search_tools:
  - search_tool_name: tavily-search
    litellm_params:
      search_provider: tavily
      api_key: os.environ/TAVILY_API_KEY

google_ai_studio_passthrough:
  api_key: os.environ/GEMINI_API_KEY

environment_variables:
  OPENAI_API_KEY: "sk-..."
  GEMINI_API_KEY: "AIza..."
  TAVILY_API_KEY: "tvly-..."
```

- `general_settings`: ポート・ログファイル・マスターキー認証設定
  - `master_key`: プロキシへのアクセスを制限するAPIキー（`os.environ/` 構文対応）
  - `litellm_key_header_name`: 認証ヘッダ名（未設定時は `Authorization`）
  - `bedrock_include_reasoning`: Bedrock 応答中の `<reasoning>...</reasoning>` を返すか（デフォルト: false）
- `model_list`: `litellm_params.model` のプレフィックス (`openai/`, `gemini/`, `ollama_chat/`, `bedrock/`) でプロバイダ判定。`api_base` はモデル毎に設定可能。Bedrock は `region` を利用
- `search_tools`: 検索ツール設定（Tavily等）。`search_provider` でプロバイダ判定
- `google_ai_studio_passthrough`: Geminiパススルー用APIキー設定
- `environment_variables`: YAMLからOS環境変数をセット（既存の環境変数が優先）
- `os.environ/VARNAME`: 環境変数参照構文（api_key等で使用）

## 設定の優先順位

環境変数 (OS) > YAML設定ファイル (model_list / environment_variables) > コマンドラインフラグ > デフォルト値

## 依存関係

- `gopkg.in/yaml.v3` - YAML設定ファイルパース
- `github.com/google/uuid` - リクエストID生成
- `github.com/aws/aws-sdk-go-v2/config` - AWS認証情報・リージョン解決
- `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` - Bedrock Runtime InvokeModel

## エラーレスポンス形式

統合エンドポイント: OpenAI形式 `{"error":{"message":"...","type":"..."}}`
パススルー: upstreamのエラーをそのまま返却
