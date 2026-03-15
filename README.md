# gollmproxy

Go製の軽量LLMプロキシ。[LiteLLM](https://github.com/BerriAI/litellm) の設定形式に互換性があり、LiteLLMからの移行やドロップイン代替として使用できる。

OpenAI・Gemini・TavilyのAPIを統一的なOpenAI互換インターフェースで利用でき、各バックエンドへのパススルーも可能。プロキシがAPIキーを注入するため、クライアント側でのキー管理が不要になる。

**LiteLLMの全機能を実装しているわけではありません。** APIキー管理・プロバイダルーティング・ログ記録など、プロキシとしての中核機能に特化しています。

## LiteLLMとの比較

| | gollmproxy | LiteLLM |
|---|---|---|
| 起動時間 | ~9 ms | ~31 s |
| メモリ使用量 | ~7 MB | ~320 MB |
| 言語 | Go (シングルバイナリ) | Python |
| 対応プロバイダ | OpenAI, Gemini, Tavily | 100+ プロバイダ |
| DB依存 | なし | PostgreSQL (optional) |
| 設定形式 | LiteLLM互換YAML | YAML |

### 実装済み機能

- OpenAI互換エンドポイント (`/v1/chat/completions`)
- プロバイダルーティング (`openai/`, `gemini/` プレフィックス)
- `model_list` によるモデルエイリアス解決
- パススルーエンドポイント (`/openai/*`, `/gemini/*`, `/tavily/*`)
- APIキー認証 (`master_key`, カスタムヘッダ名)
- SSEストリーミング (Gemini→OpenAI形式変換)
- リクエストログ (JSONL, user/metadata/トークン使用量)
- `os.environ/` 構文による環境変数参照

### 未実装の主なLiteLLM機能

- Anthropic, Azure, Bedrock, Vertex AI 等のプロバイダ
- ロードバランシング・フォールバック
- レートリミット・バジェット管理
- キャッシング
- Embeddings / Image Generation / Audio エンドポイント
- Web UI (Admin Panel)

## 機能

- **高速起動** - 0.1秒以内に起動完了（環境による）
- **省メモリ** - 起動直後のメモリ使用量10MB以内（ステートレス設計のため長時間運用でも安定）
- **OpenAI互換エンドポイント** (`/v1/chat/completions`) - プロバイダプレフィックスでバックエンド自動ルーティング
- **パススルーエンドポイント** (`/openai/*`, `/gemini/*`, `/tavily/*`) - 各APIへそのまま転送
- **LiteLLM互換設定** - `model_list`, `general_settings`, `os.environ/` 構文に対応
- **APIキー認証** - `master_key` によるプロキシアクセス制限
- **APIキー注入** - 環境変数で設定したキーをリクエストに自動挿入
- **SSEストリーミング対応** - Geminiレスポンスも自動でOpenAI SSE形式に変換
- **リクエストログ** - JSONL形式でリクエスト/レスポンスを記録（user, metadata, トークン使用量対応）

## クイックスタート

### ビルド

```bash
go build -o gollmproxy .
```

### APIキーの設定

```bash
export OPENAI_API_KEY="sk-..."
export GEMINI_API_KEY="AIza..."
export TAVILY_API_KEY="tvly-..."
```

### 起動

```bash
# デフォルト設定 (ポート8080, ログ: gollmproxy.log)
./gollmproxy

# ポート指定
./gollmproxy -port 9090

# 設定ファイル指定
./gollmproxy -config config.yaml
```

## 使い方

### OpenAI互換エンドポイント

`model` フィールドに `プロバイダ/モデル名` を指定することでバックエンドが自動選択される。

```bash
# Gemini経由
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini/gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# OpenAI経由
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# ストリーミング
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini/gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Tell me a story"}],
    "stream": true
  }'

# user / metadata 付き
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini/gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Hello!"}],
    "user": "user_12345",
    "metadata": {"org_code": "org_abc", "app_name": "my_app"}
  }'
```

プレフィックスなしの場合はOpenAIとして扱われる。

### パススルーエンドポイント

各バックエンドAPIにそのままリクエストを転送する。APIキーのみ自動挿入される。

```bash
# OpenAI - モデル一覧
curl http://localhost:8080/openai/v1/models

# Gemini - 直接呼び出し
curl -X POST http://localhost:8080/gemini/v1beta/models/gemini-2.5-flash:generateContent \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{"parts": [{"text": "Hello"}]}]
  }'

# Tavily - Web検索
curl -X POST http://localhost:8080/tavily/search \
  -H "Content-Type: application/json" \
  -d '{"query": "latest AI news"}'
```

### ヘルスチェック

```bash
curl http://localhost:8080/health
# => {"status":"ok"}
```

## 設定

### 環境変数

| 変数名 | 説明 | デフォルト |
|--------|------|-----------|
| `PORT` | サーバーポート | `8080` |
| `LOG_FILE` | リクエストログファイルパス | `gollmproxy.log` |
| `LITELLM_MASTER_KEY` | プロキシ認証用マスターキー | (なし) |
| `OPENAI_API_KEY` | OpenAI APIキー | (なし) |
| `GEMINI_API_KEY` | Gemini APIキー | (なし) |
| `TAVILY_API_KEY` | Tavily APIキー | (なし) |
| `OPENAI_BASE_URL` | OpenAI APIベースURL | `https://api.openai.com` |
| `GEMINI_BASE_URL` | Gemini APIベースURL | `https://generativelanguage.googleapis.com` |
| `TAVILY_BASE_URL` | Tavily APIベースURL | `https://api.tavily.com` |

### 設定ファイル (LiteLLM互換YAML)

`-config` フラグで指定。LiteLLMと同じ形式の設定ファイルを使用できる。

```yaml
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  litellm_key_header_name: X-Litellm-Api-Key
  port: 8080
  log_file: "gollmproxy.log"
  log_request_body: true
  log_response_body: true

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

- `general_settings`: ポート・ログファイル・認証・ログ出力設定
  - `master_key`: プロキシへのアクセスを制限するAPIキー（`os.environ/` 構文対応）
  - `litellm_key_header_name`: 認証ヘッダ名（未設定時は `Authorization`）
  - `log_request_body`: リクエストボディをログに記録するか（デフォルト: `true`）
  - `log_response_body`: レスポンスボディをログに記録するか（デフォルト: `true`）
- `model_list`: LiteLLM互換。`litellm_params.model` のプレフィックス (`openai/`, `gemini/`) でプロバイダ判定
- `search_tools`: 検索ツール設定（Tavily等）
- `google_ai_studio_passthrough`: Geminiパススルー用APIキー設定
- `environment_variables`: YAMLからOS環境変数をセット（既存の環境変数が優先）
- `os.environ/VARNAME`: LiteLLM互換の環境変数参照構文（`api_key` 等で使用）

### 認証

`master_key` を設定すると、プロキシへのリクエストにAPIキー認証が必要になる（`/health` を除く）。

```bash
# Authorization ヘッダで認証
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-master-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gemini/gemini-2.5-flash", "messages": [{"role": "user", "content": "Hello!"}]}'

# カスタムヘッダ名を設定した場合 (litellm_key_header_name: X-Litellm-Api-Key)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "X-Litellm-Api-Key: your-master-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gemini/gemini-2.5-flash", "messages": [{"role": "user", "content": "Hello!"}]}'
```

### 優先順位

環境変数 > YAML設定ファイル > コマンドラインフラグ > デフォルト値

## ログ

リクエストログはJSONL形式 (1行1リクエスト) で記録される。

```bash
tail -f gollmproxy.log | jq .
```

各エントリのフィールド:

| フィールド | 説明 |
|-----------|------|
| `timestamp` | リクエスト受信時刻 (RFC3339) |
| `request_id` | ユニークなリクエストID |
| `method` | HTTPメソッド |
| `path` | リクエストパス |
| `user` | リクエストの `user` フィールド |
| `metadata` | リクエストの `metadata` オブジェクト |
| `model` | 使用モデル名 |
| `provider` | バックエンドプロバイダ (openai, gemini) |
| `stream` | ストリーミングリクエストかどうか |
| `status_code` | レスポンスステータスコード |
| `latency_ms` | レイテンシ (ミリ秒) |
| `prompt_tokens` | プロンプトトークン数 |
| `completion_tokens` | 生成トークン数 |
| `total_tokens` | 合計トークン数 |
| `req_body` | リクエストボディ (最大10KB, 設定で無効化可) |
| `resp_body` | レスポンスボディ (最大10KB, 設定で無効化可) |
| `client_ip` | クライアントIPアドレス |

トークン使用量は非ストリーミングレスポンスで記録される。

## ライセンス

[MIT](LICENSE)
