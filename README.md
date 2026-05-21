# gollmproxy

Go製の軽量LLMプロキシ。OpenAI・Gemini・Bedrock・TavilyのAPIを統一的なOpenAI互換インターフェースで利用でき、各バックエンドへのパススルーも可能。プロキシがAPIキーやAWS認証情報を利用してバックエンドへ中継するため、クライアント側での管理が不要になる。[LiteLLM](https://github.com/BerriAI/litellm) 互換の設定形式に対応しており、移行やドロップイン代替として使用できる。

APIキー管理・プロバイダルーティング・ログ記録など、プロキシとしての中核機能に特化している。

## 機能

- **高速起動** - 0.1秒以内に起動完了（環境による）
- **省メモリ** - 起動直後のメモリ使用量10MB以内（ステートレス設計のため長時間運用でも安定）
- **OpenAI互換エンドポイント** (`/v1/chat/completions`) - プロバイダプレフィックスでバックエンド自動ルーティング
- **パススルーエンドポイント** (`/openai/*`, `/gemini/*`, `/tavily/*`) - 各APIへそのまま転送
- **Bedrock推論対応** - `bedrock/<model-id>` で AWS SDK InvokeModel 経由、`bedrock_openai/<model-id>` で Bedrock OpenAI 互換エンドポイント経由（いずれも SigV4 認証）
- **YAML設定ファイル** - `model_list`, `general_settings`, `os.environ/` 構文に対応
- **APIキー認証** - `master_key` によるプロキシアクセス制限
- **APIキー注入** - 環境変数で設定したキーをリクエストに自動挿入
- **SSEストリーミング対応** - Geminiレスポンスも自動でOpenAI SSE形式に変換
- **リクエストログ** - JSONL形式でリクエスト/レスポンスを記録（user, metadata, トークン使用量対応）
- **メタデータ必須バリデーション** - `required_metadata_keys` で指定したキーが `metadata` に存在しない場合は 400 エラー

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
export AWS_REGION="ap-northeast-1"
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

# Bedrock経由 (AWS SDK InvokeModel)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bedrock/openai.gpt-oss-20b-1:0",
    "messages": [{"role": "user", "content": "Hello, gpt-oss-20b!"}]
  }'

# Bedrock経由 (OpenAI互換エンドポイント / SigV4署名)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bedrock_openai/anthropic.claude-3-5-sonnet-20241022-v2:0",
    "messages": [{"role": "user", "content": "Hello, Claude on Bedrock!"}]
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

**Bedrock の2つのモード:**

| プレフィックス | 方式 | 用途 |
|---------------|------|------|
| `bedrock/` | AWS SDK InvokeModel | AWS独自形式のモデル (`openai.gpt-oss-*` 等) |
| `bedrock_openai/` | Bedrock OpenAI互換エンドポイント (SigV4署名) | Claude等のOpenAI互換対応モデル |

`bedrock/` は `InvokeModel` と `InvokeModelWithResponseStream` の両方に対応。`bedrock_include_reasoning: false` の場合は `<reasoning>...</reasoning>` を除去して返す。`bedrock_openai/` は `api_base` でVPCエンドポイント等のカスタムURLも指定可能。

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
| `AWS_REGION` | Bedrock 実行リージョン | (なし) |
| `AWS_DEFAULT_REGION` | Bedrock 実行リージョンの代替 | (なし) |
| `OPENAI_BASE_URL` | OpenAI APIベースURL | `https://api.openai.com` |
| `GEMINI_BASE_URL` | Gemini APIベースURL | `https://generativelanguage.googleapis.com` |
| `TAVILY_BASE_URL` | Tavily APIベースURL | `https://api.tavily.com` |
| `TOKEN_BUDGET_ENABLED` | app_id/model_name 単位のトークン予算管理を有効化 | `false` |

### 設定ファイル (YAML)

`-config` フラグで指定。

```yaml
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  litellm_key_header_name: X-Litellm-Api-Key
  port: 8080
  log_file: "gollmproxy.log"
  log_request_body: true
  log_response_body: true
  token_budget_enabled: false
  bedrock_include_reasoning: false
  required_metadata_keys:
    - app_id
    - user_id

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
  - model_name: claude-sonnet-bedrock
    litellm_params:
      model: bedrock_openai/anthropic.claude-3-5-sonnet-20241022-v2:0
      region: us-east-1

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
  AWS_REGION: "ap-northeast-1"
```

- `general_settings`: ポート・ログファイル・認証・ログ出力設定
  - `master_key`: プロキシへのアクセスを制限するAPIキー（`os.environ/` 構文対応）
  - `litellm_key_header_name`: 認証ヘッダ名（未設定時は `Authorization`）
  - `log_request_body`: リクエストボディをログに記録するか（デフォルト: `true`）
  - `log_response_body`: レスポンスボディをログに記録するか（デフォルト: `true`）
  - `token_budget_enabled`: `/v1/chat/completions` の予算管理を有効化するか（デフォルト: `false`）。`metadata.app_id` とログ保存時の `model_name` を使って判定する
  - `bedrock_include_reasoning`: Bedrock 応答中の `<reasoning>...</reasoning>` をそのまま返すか。未設定時は `false`
  - `required_metadata_keys`: `/v1/chat/completions` の `metadata` フィールドで必須とするキーのリスト。指定したキーが存在しないまたは空の場合は HTTP 400 を返す
- `model_list`: `litellm_params.model` のプレフィックス (`openai/`, `gemini/`, `bedrock/`, `bedrock_openai/`) でプロバイダ判定
- Bedrock 利用時 (`bedrock/`, `bedrock_openai/` 共通) は `litellm_params.region` を優先し、未指定なら `AWS_REGION` または `AWS_DEFAULT_REGION` を利用。`bedrock_openai/` は `api_base` でエンドポイントURLを上書き可能
- `model_name` がある場合の個別設定は `model_name` 単位で保持されるため、同じ `litellm_params.model` を複数の別名で使っても `region` や `api_base` は上書きされない
- PostgreSQL ログの `model_name` 列と token budget 判定には、`model_list.model_name` で指定した値（またはクライアントが直接指定した `model` 値）が使われる
- PostgreSQL ログの `metadata.litellm_params` には、`litellm_params` のうち `model` / `api_base` / `region` / `search_provider` だけが保存される。`api_key` やその他のキーは保存されない
- `search_tools`: 検索ツール設定（Tavily等）
- `google_ai_studio_passthrough`: Geminiパススルー用APIキー設定
- `environment_variables`: YAMLからOS環境変数をセット（既存の環境変数が優先）
- `os.environ/VARNAME`: 環境変数参照構文（`api_key` 等で使用）

### トークン予算管理（app_id/model_name 単位）

`general_settings.token_budget_enabled: true`（または `TOKEN_BUDGET_ENABLED=true`）時、`/v1/chat/completions` で invoke 前に日次予算チェックを行う。

- `metadata.app_id` が未指定または空の場合は 429
- ログ保存に使う `model_name` が空の場合は 429
- `token_budgets` テーブルに予算設定が無い場合は 429
- 当日の累計が予算以上の場合は 429

#### 重要: ソフトリミット

予算はソフトリミットで、invoke 後に `usage.total_tokens` を `token_usage_daily` へ upsert 加算するため、1リクエストで予算超過することは許容される。
また、同一 `app_id/model_name` への同時リクエストが複数ある場合、事前チェックが同時に通過して超過量が大きくなる可能性がある。
厳密な上限制御が必要な場合は、DBロック（例: advisory lock）や更新時の競合制御を追加する運用を推奨。

```sql
CREATE TABLE token_budgets (
  app_id text NOT NULL,
  model_name text NOT NULL,
  token_budget bigint NOT NULL CHECK (token_budget >= 0),
  PRIMARY KEY (app_id, model_name)
);

CREATE TABLE token_usage_daily (
  usage_date date NOT NULL,
  app_id text NOT NULL,
  model_name text NOT NULL,
  token bigint NOT NULL CHECK (token >= 0),
  PRIMARY KEY (usage_date, app_id, model_name)
);
```

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
| `provider` | バックエンドプロバイダ (openai, gemini, bedrock など) |
| `stream` | ストリーミングリクエストかどうか |
| `status_code` | レスポンスステータスコード |
| `latency_ms` | レイテンシ (ミリ秒) |
| `prompt_tokens` | プロンプトトークン数 |
| `completion_tokens` | 生成トークン数 |
| `total_tokens` | 合計トークン数 |
| `req_body` | リクエストボディ (最大10KB, 設定で無効化可) |
| `resp_body` | レスポンスボディ (最大10KB, 設定で無効化可、非ストリーミングのみ) |
| `client_ip` | クライアントIPアドレス |

トークン使用量は非ストリーミングレスポンスで記録される。

### ストリーミング時のチャンクログ

`log_response_body: true`（デフォルト）の場合、SSEストリーミングではレスポンスボディをメモリに蓄積せず、チャンクごとに個別のJSONLエントリとして即座に書き出す。サマリログの `resp_body` は空になる。

チャンクログのフィールド:

| フィールド | 説明 |
|-----------|------|
| `timestamp` | チャンク送信時刻 (RFC3339) |
| `request_id` | 対応するリクエストのID |
| `chunk_index` | チャンクの連番 (0始まり) |
| `data` | SSEチャンクのデータ (最大10KB) |

```bash
# チャンクログのみ抽出
jq 'select(.chunk_index != null)' gollmproxy.log

# サマリログのみ抽出
jq 'select(.chunk_index == null)' gollmproxy.log
```

### PostgreSQLへのログ出力

JSONLファイルに加えて、PostgreSQLへリクエストログを書き込める。モデル・トークン・メタデータは `llm_logs` に、リクエスト/レスポンスボディ全文は `llm_payloads` に格納される。書き込みは非同期（バッファ付きキュー）のため、リクエスト処理のレイテンシに影響しない。`/v1/chat/completions` の `model_name` 列には、`model_list.model_name` で設定した名前（別名を使わない場合はクライアントが指定した `model` 値）が保存される。

#### テーブル作成

```sql
CREATE TABLE llm_logs (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  created_at  timestamptz DEFAULT now(),
  model_name  text        NOT NULL,
  input_tokens  int,
  output_tokens int,
  metadata    jsonb,
  -- metadata 内の頻出キーを生成列として独立させてインデックスを張りやすくする
  app_id   text GENERATED ALWAYS AS (metadata->>'app_id')   STORED,
  dept_cd  text GENERATED ALWAYS AS (metadata->>'dept_cd')  STORED,
  user_id  text GENERATED ALWAYS AS (metadata->>'user_id')  STORED
);

-- metadata 全体を GIN インデックスで検索可能にする
CREATE INDEX idx_llm_logs_metadata ON llm_logs USING gin (metadata);

-- リクエスト/レスポンスボディ全文（デバッグ・監査用）
CREATE TABLE llm_payloads (
  log_id      uuid  PRIMARY KEY REFERENCES llm_logs(id) ON DELETE CASCADE,
  input_body  jsonb,
  output_body jsonb
);
```

`metadata` には `provider`, `path`, `status_code`, `latency_ms`, `user`, `client_ip` 等が自動的に格納される。リクエスト時に指定した `metadata` オブジェクトのキー（`app_id`, `dept_cd`, `user_id` 等）も同じカラムにマージされる。

さらに `/v1/chat/completions` では、サーバー設定由来の `litellm_params` 情報が `metadata.litellm_params` に追加される。保存されるのは次のホワイトリストだけで、`api_key` やその他の設定値は保存されない。

```json
{
  "litellm_params": {
    "model": "openai/gpt-4o",
    "api_base": "https://api.openai.com",
    "region": "ap-northeast-1",
    "search_provider": "tavily"
  }
}
```

`metadata.litellm_params` はサーバーが生成する予約領域で、同じキーがリクエスト metadata に含まれていてもサーバー設定の値で上書きされる。

#### 接続設定

**方法1: 接続文字列 (POSTGRES_DSN)**

環境変数または YAML で指定する。

```bash
export POSTGRES_DSN="postgres://proxy_admin:password@db.example.com:5432/llm_gateway_logs?sslmode=require"
```

```yaml
general_settings:
  postgres_dsn: os.environ/POSTGRES_DSN
```

**方法2: 個別の PG* 環境変数**

`POSTGRES_DSN` が未設定の場合、標準の libpq 環境変数を自動的に使用する。

| 環境変数 | 説明 | 例 |
|---------|------|---|
| `PGHOST` | ホスト名 | `db.example.com` |
| `PGPORT` | ポート番号 | `5432` |
| `PGUSER` | ユーザー名 | `proxy_admin` |
| `PGPASSWORD` | パスワード | `(secret)` |
| `PGDATABASE` | データベース名 | `llm_gateway_logs` |
| `PGSSLMODE` | SSLモード | `require` |

```bash
export PGHOST=db.example.com
export PGPORT=5432
export PGUSER=proxy_admin
export PGPASSWORD=secret_password
export PGDATABASE=llm_gateway_logs
export PGSSLMODE=require
./gollmproxy -config config.yaml
```

どちらの方法も未設定の場合、PostgreSQL へのログ出力は行われず JSONL ファイルのみに書き込まれる。

#### クエリ例

```sql
-- 直近1時間のリクエスト数とトークン使用量をモデル別に集計
SELECT
  model_name,
  count(*)              AS requests,
  sum(input_tokens)     AS total_input_tokens,
  sum(output_tokens)    AS total_output_tokens
FROM llm_logs
WHERE created_at > now() - interval '1 hour'
GROUP BY model_name
ORDER BY requests DESC;

-- app_id ごとのコスト試算用集計
SELECT
  app_id,
  count(*)          AS requests,
  sum(input_tokens) AS input_tokens,
  sum(output_tokens) AS output_tokens
FROM llm_logs
WHERE app_id IS NOT NULL
GROUP BY app_id;

-- app_id / model_name ごとの日次集計
SELECT
  app_id,
  model_name,
  count(*) AS requests,
  sum(input_tokens) AS input_tokens,
  sum(output_tokens) AS output_tokens
FROM llm_logs
WHERE created_at >= date_trunc('day', now())
GROUP BY app_id, model_name
ORDER BY app_id, model_name;

-- エラーになったリクエストのボディを確認
SELECT l.id, l.model_name, l.created_at,
       p.input_body, p.output_body
FROM llm_logs l
JOIN llm_payloads p ON p.log_id = l.id
WHERE (l.metadata->>'status_code')::int >= 400
ORDER BY l.created_at DESC
LIMIT 20;
```

## ライセンス

[MIT](LICENSE)
