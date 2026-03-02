# Webhook Hub & Archiver

LINE Messaging API の Webhook を受信・アーカイブし、複数の子 Webhook クライアントへ非同期転送する軽量ハブです。

## 特徴

- `POST /callback` で LINE Webhook を受信
- `x-line-signature` と `LINE_CHANNEL_SECRET` による HMAC-SHA256 署名検証
- SQLite3 に Webhook ペイロードをアーカイブ（`webhooks` テーブル）
- `clients` テーブルで定義された複数の転送先へ非同期 HTTP POST
- 転送結果を構造化 JSON ログ（`log/slog`）として標準出力に出力
- `GET /health` のヘルスチェックエンドポイント

詳細仕様は `spec.md` を参照してください。

## 必要要件

- Go 1.22+
- Docker / Docker Compose

## 環境変数

- `LINE_CHANNEL_SECRET` (必須): LINE チャネルシークレット。署名検証に使用します。
- `DB_PATH` (任意): SQLite DB ファイルパス。デフォルトは `/data/webhook.db`。
- `PORT` (任意): HTTP リッスンポート。デフォルトは `8080`。

## 起動方法（Docker Compose）

```bash
export LINE_CHANNEL_SECRET=your_channel_secret
docker compose up --build
```

起動後:

- Webhook 受信: `POST http://localhost:8080/callback`
- ヘルスチェック: `GET http://localhost:8080/health`

### Cloudflare Tunnel (cloudflared)

`docker compose up` で **cloudflared** も起動し、webhook-hub をインターネットに公開します。

- **クイックトンネル（デフォルト）**: 起動後、cloudflared のログに `https://xxxxx.trycloudflare.com` のような URL が表示されます。LINE の Webhook URL にこのアドレス + `/callback` を設定してください。
- **名前付きトンネル**: 固定ドメインを使う場合は、Cloudflare Zero Trust でトンネルを作成し発行されたトークンを `.env` の `CLOUDFLARE_TUNNEL_TOKEN` に設定。`docker-compose.yml` の cloudflared の `command` を `tunnel run --token $$CLOUDFLARE_TUNNEL_TOKEN` に変更して起動してください。

## clients テーブルの管理

初期バージョンでは、`clients` テーブルは手動で管理します。例:

```sql
INSERT INTO clients (name, webhook_url, is_active)
VALUES ('sample-bot', 'http://child-bot:8000/hook', 1);
```

将来的に API 経由での管理を追加する余地がありますが、現時点ではスコープ外です。

