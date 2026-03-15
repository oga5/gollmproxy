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
openai_compat.go              POST /v1/chat/completions ハンドラ (プロバイダ振り分け, OpenAI/Gemini処理)
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
  │       └─ provider=gemini → convert_openai_to_gemini.go で変換 → Gemini API
  │                             → convert_gemini_to_openai.go でレスポンス変換
  │
  ├─ /openai/* ──→ passthrough.go → api.openai.com (Authorization: Bearer)
  ├─ /gemini/* ──→ passthrough.go → generativelanguage.googleapis.com (?key=)
  └─ /tavily/* ──→ passthrough.go → api.tavily.com (Authorization: Bearer)
```

### ミドルウェアチェーン

```
recoveryMiddleware → requestIDMiddleware → loggingMiddleware → ルーターハンドラ
```

- **recovery**: パニック捕捉、500レスポンス
- **requestID**: UUID生成、X-Request-IDヘッダ、コンテキスト注入
- **logging**: ステータスコード・レイテンシ記録

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

## 設定の優先順位

環境変数 > YAML設定ファイル (-config) > コマンドラインフラグ > デフォルト値

## 依存関係

- `gopkg.in/yaml.v3` - YAML設定ファイルパース
- `github.com/google/uuid` - リクエストID生成
- その他は Go 標準ライブラリのみ

## エラーレスポンス形式

統合エンドポイント: OpenAI形式 `{"error":{"message":"...","type":"..."}}`
パススルー: upstreamのエラーをそのまま返却
