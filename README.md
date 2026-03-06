# Webhook Hub & Archiver

LINE Messaging API の Webhook を受信・アーカイブし、複数の子 Webhook クライアントへ非同期転送する軽量ハブです。

## 特徴

- `POST /callback` で LINE Webhook を受信
- `x-line-signature` と `LINE_CHANNEL_SECRET` による HMAC-SHA256 署名検証
- SQLite3 に Webhook ペイロードをアーカイブ（`webhooks` テーブル）
- 登録された転送先 URL 一覧（JSON ファイル + メモリ）へ非同期 HTTP POST
- 転送結果を構造化 JSON ログ（`log/slog`）として標準出力に出力
- `GET /health` のヘルスチェックエンドポイント
- **clients 管理の REST API**: `GET /clients`（一覧）、`POST /clients`（登録）、`DELETE /clients`（削除）

## 必要要件

- Go 1.22+
- Docker / Docker Compose

## 環境変数

- `LINE_CHANNEL_SECRET` (必須): LINE チャネルシークレット。署名検証に使用します。
- `DB_PATH` (任意): SQLite DB ファイルパス。デフォルトは `/data/webhook.db`。
- `CLIENTS_FILE` (任意): 転送先 URL 一覧を保存する JSON ファイルパス。デフォルトは `/data/clients.json`。起動時に読み込み、追加時に書き戻します。
- `PORT` (任意): HTTP リッスンポート。デフォルトは `8080`。

## 起動方法（Docker Compose）

```bash
export LINE_CHANNEL_SECRET=your_channel_secret
docker compose up --build
```

起動後:

- Webhook 受信: `POST http://localhost:8080/callback`
- ヘルスチェック: `GET http://localhost:8080/health`
- クライアント一覧: `GET http://localhost:8080/clients`
- クライアント登録: `POST http://localhost:8080/clients`
- クライアント削除: `DELETE http://localhost:8080/clients`（下記参照）

## clients 管理 REST API

転送先は **Webhook URL の配列** のみ管理します。起動時に `CLIENTS_FILE`（JSON）を読みメモリに保持し、リクエストごとの参照はメモリのみ。追加時に JSON へ書き戻して永続化します。

### 一覧取得 `GET /clients`

登録済み転送先 URL の配列を JSON で返します。

```bash
curl -s http://localhost:8080/clients
# 例: ["http://my-client:8080/webhook"]
```

### 登録 `POST /clients`

転送先 URL を 1 件追加します。Body は JSON で `webhook_url` が必須です。

```bash
curl -s -X POST http://localhost:8080/clients \
  -H "Content-Type: application/json" \
  -d '{"webhook_url":"http://my-client:8080/webhook"}'
```

### 削除 `DELETE /clients`

転送先 URL を 1 件削除します。Body は JSON で `webhook_url` を指定します。存在しない URL の場合は `404 Not Found` を返します。削除成功時は `204 No Content` です。

```bash
curl -s -X DELETE http://localhost:8080/clients \
  -H "Content-Type: application/json" \
  -d '{"webhook_url":"http://my-client:8080/webhook"}'
```

