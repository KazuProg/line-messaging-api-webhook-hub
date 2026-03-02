# Webhook Hub & Archiver 技術仕様書

## 1. 概要

本システムは、LINE Messaging APIから送信されるWebhookイベントを受信・保存し、登録された複数の子Webhookクライアント（外部サービス/コンテナ）へ非同期に配信する「軽量ハブ」である。

## 2. システム構成

* **Runtime**: Go 1.22+ (Alpine Linux)
* **Database**: SQLite 3 (Write-Ahead Logging推奨)
* **Container**: Docker / Docker Compose
* **永続化**: ホストボリュームマウントによるDBファイル永続化

---

## 3. インフラ構成 (Deployment)

### 3.1 Docker構成

マルチステージビルドを採用し、実行環境は `alpine:latest` を使用してイメージサイズを20MB程度に軽量化する。

### 3.2 Docker Compose 仕様

```yaml
services:
  webhook-hub:
    build: .
    container_name: webhook-hub
    ports:
      - "8080:8080"
    environment:
      - LINE_CHANNEL_SECRET=${LINE_CHANNEL_SECRET}
      - DB_PATH=/data/webhook.db
    volumes:
      - ./data:/data  # SQLiteデータの永続化マウント
    restart: always
    networks:
      - bot-network

networks:
  bot-network:
    driver: bridge

```

---

## 4. データ構造 (SQLite)

### `webhooks` テーブル（アーカイブ用）

| カラム名 | 型 | 制約 | 説明 |
| --- | --- | --- | --- |
| `id` | TEXT | PRIMARY KEY | LINEの `webhookEventId` (重複排除に使用) |
| `payload` | TEXT | NOT NULL | 受信したJSONデータ全文 |

### `clients` テーブル（転送先管理）

| カラム名 | 型 | 制約 | 説明 |
| --- | --- | --- | --- |
| `id` | INTEGER | PRIMARY KEY | 内部ID |
| `name` | TEXT | NOT NULL | 子コンテナ名等の識別子 |
| `webhook_url` | TEXT | NOT NULL | 転送先URL（例: http://child-bot:8000/hook） |
| `is_active` | INTEGER | DEFAULT 1 | 有効(1) / 無効(0) |

---

## 5. アプリケーション仕様

### 5.1 処理フロー

1. **検証**: HTTPヘッダー `x-line-signature` を `LINE_CHANNEL_SECRET` を鍵としたHMAC-SHA256で検証。
2. **保存**: 受信したJSONを `webhooks` テーブルへ `INSERT OR IGNORE`。
3. **転送 (非同期)**:
* `clients` テーブルから有効なURLを全取得。
* `Go Goroutine` を用いて、各URLへ同一ペイロードをHTTP POST（Fire and Forget）。
* リトライ処理は行わず、結果はログ出力に留める。


4. **返答**: LINEプラットフォームに対し、速やかに `200 OK` を返却。

### 5.2 ログ出力

`log/slog` パッケージを用い、以下の構造化JSONログを標準出力する。

* **Success**: `{"time":..., "level":"INFO", "msg":"forwarding success", "event_id":..., "url":...}`
* **Failure**: `{"time":..., "level":"ERROR", "msg":"forwarding failed", "event_id":..., "error":...}`

---

## 6. 実装上の注意点

* **SQLiteの同時実行**: 受信と転送処理が重なる可能性があるため、DB接続時に `_journal_mode=WAL` を有効にすることが望ましい。
* **ネットワーク**: 子コンテナは同一の `docker-network` 内に配置し、`webhook_url` にはコンテナ名ドメインを使用すること。
* **署名検証**: 外部AI Agentが実装する際、`crypto/hmac` と `encoding/base64` を正しく使用してLINEの仕様に準拠させること。

---

## 7. 追加・詳細仕様 (Appendix)

### 7.1 エンドポイントとルーティング

* **Webhook受信パス**: `POST /callback`
* LINE Platformからのリクエストをこのパスで集約する。
* **必須ヘッダー**: `x-line-signature`（署名検証用）

### 7.2 clients の登録・管理

* **管理方法**: 初期段階では手動SQL（外部管理ツールやCLI）または `docker-compose.yml` での初期データ投入を想定する。
* API経由の管理機能は将来拡張とし、本バージョンのスコープには含めない。
* 起動時に `clients` テーブルが空の場合は、警告ログを出力し、受信データの保存のみを行う（転送はスキップ）。

### 7.3 署名検証とエラーハンドリング

**検証手順**:

1. リクエストBodyの生バイト列を取得する。
2. `LINE_CHANNEL_SECRET` を鍵として、Bodyのバイト列から `HMAC-SHA256` ハッシュを生成する。
3. 生成されたハッシュを `Base64` エンコードする。
4. エンコードした値と、ヘッダーの `x-line-signature` を定数時間比較で照合する。

**検証失敗時の挙動**:

* `HTTP 401 Unauthorized` を返し、処理を中断する。
* ログには `WARNING` レベルで不正アクセスを記録する。

### 7.4 `webhookEventId` の抽出

* **取得元**: JSON内の `$.events[0].webhookEventId`
* LINEのWebhookは複数のイベントを1つのリクエストに含めることがあるが、本システムでは最初（index 0）の `webhookEventId` をリクエスト全体の代表IDとして扱う。
* `events` 配列が空、または `webhookEventId` が取得できない場合は、`"unknown-" + Unixタイムスタンプ(ミリ秒)` の形式で代替IDを生成して保存する。

### 7.5 データベースのライフサイクル

* **初期化**: アプリケーション起動時に `CREATE TABLE IF NOT EXISTS` を実行し、`webhooks`・`clients` の両テーブルが存在しない場合は自動作成する。
* **マイグレーション**: スキーマ変更が必要になった場合は、起動時に実行するSQLを拡張することで対応する（専用マイグレーションツールは本スコープ外）。

### 7.6 転送処理の通信仕様

* **Content-Type**: `application/json; charset=utf-8`
* **タイムアウト**: 各転送先ごとに 5秒（HTTPクライアントのコンテキストタイムアウトとして設定）。
* 子コンテナがハングアップしていても、タイムアウトによってGoroutineを解放し、リソース枯渇を防ぐ。
* **ペイロード**: LINEから受信したBodyを一切変更せず、そのまま転送する（透過転送）。

### 7.7 サーバー設定・その他

* **リッスンポート**: デフォルトは `8080`。環境変数 `PORT` が指定されている場合はその値を優先する。
* **最大ボディサイズ**: LINEのWebhook仕様に準拠しつつ、安全のため上限を 1MB 程度に制限する（超過時は 413 Payload Too Large を返却）。
* **ヘルスチェック**: `GET /health` に対して `200 OK`・固定JSON（例: `{"status":"ok"}`）を返す簡易ヘルスチェックエンドポイントを提供する。
