# gollmproxy

LiteLLM互換の軽量LLMプロキシ。Go製。

OpenAI・Gemini・TavilyのAPIを統一的なOpenAI互換インターフェースで利用でき、各バックエンドへのパススルーも可能。プロキシがAPIキーを注入するため、クライアント側でのキー管理が不要になる。

## 機能

- **OpenAI互換エンドポイント** (`/v1/chat/completions`) - プロバイダプレフィックスでバックエンド自動ルーティング
- **パススルーエンドポイント** (`/openai/*`, `/gemini/*`, `/tavily/*`) - 各APIへそのまま転送
- **APIキー注入** - 環境変数で設定したキーをリクエストに自動挿入
- **SSEストリーミング対応** - Geminiレスポンスも自動でOpenAI SSE形式に変換
- **リクエストログ** - JSONL形式でリクエスト/レスポンスを記録

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
| `OPENAI_API_KEY` | OpenAI APIキー | (なし) |
| `GEMINI_API_KEY` | Gemini APIキー | (なし) |
| `TAVILY_API_KEY` | Tavily APIキー | (なし) |
| `OPENAI_BASE_URL` | OpenAI APIベースURL | `https://api.openai.com` |
| `GEMINI_BASE_URL` | Gemini APIベースURL | `https://generativelanguage.googleapis.com` |
| `TAVILY_BASE_URL` | Tavily APIベースURL | `https://api.tavily.com` |

### 設定ファイル (YAML)

`-config` フラグで指定。環境変数が優先される。

```yaml
port: 8080
log_file: "gollmproxy.log"
openai_api_key: "sk-..."
gemini_api_key: "AIza..."
tavily_api_key: "tvly-..."
```

設定例は `config.yaml.example` を参照。

### 優先順位

環境変数 > YAML設定ファイル > デフォルト値

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
| `provider` | バックエンドプロバイダ (openai, gemini, tavily) |
| `model` | 使用モデル名 |
| `stream` | ストリーミングリクエストかどうか |
| `status_code` | レスポンスステータスコード |
| `latency_ms` | レイテンシ (ミリ秒) |
| `req_body` | リクエストボディ (最大10KB) |
| `resp_body` | レスポンスボディ (最大10KB) |

## ライセンス

[MIT](LICENSE)
