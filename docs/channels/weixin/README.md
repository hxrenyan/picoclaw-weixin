# 💬 Weixin (WeChat Personal) Channel

PicoClaw connects WeChat personal accounts through the Tencent iLink API. One weixin channel can host multiple bots. Each bot has its own QR login, long-poll loop, and conversation, following the multi-link model used by [Wechatai](https://github.com/moutsea/wechatai).

What changed, what it does, and the limits are written up in [多 Bot 改动说明](multi-bot.zh.md).

面向多个独立用户提供服务时，见 [用户数据与工具隔离](isolation.md)。外部扫码绑定成功后会自动开启该渠道的隔离模式；已有渠道可配置 `settings.isolate_accounts: true`。

## 🚀 Quick Onboarding

The easiest way to set up the Weixin channel is using the interactive onboarding command:

```bash
picoclaw auth weixin
```

Add another bot:

```bash
picoclaw auth weixin --account shop --name "Shop bot"
picoclaw auth weixin list
```

This command will:
1. Request a QR code from the iLink API and display it in your terminal.
2. Wait for you to scan it from WeChat: **Settings → Plugins → WeChat ClawBot → Scan**.
3. Save that bot's token. An empty `--account` updates the default bot. A new key adds another bot.

After onboarding, you can start the gateway:

```bash
picoclaw gateway
```

---

## ⚙️ Configuration

You can also manually configure the filter rules in `config.json` under the `channels.weixin` section.

```json
{
  "channel_list": {
    "weixin": {
      "enabled": true,
      "type": "weixin",
      "allow_from": ["user_id_1"],
      "settings": {
        "token": "DEFAULT_BOT_TOKEN",
        "account_id": "DEFAULT_BOT_ID",
        "user_id": "DEFAULT_USER_ID",
        "accounts": {
          "shop": {
            "name": "Shop bot",
            "token": "SHOP_BOT_TOKEN",
            "account_id": "SHOP_BOT_ID",
            "user_id": "SHOP_USER_ID"
          }
        }
      }
    }
  }
}
```

The default bot keeps the original `token` and `account_id`, so existing configs do not need a migration. Extra bots live under `settings.accounts`. Keys use letters, numbers, `_`, or `-`. The bots share one poller and one HTTP connection pool. When WeChat serves HTTP/2, those long-polls share one connection. Each bot still has its own token and one in-flight getupdates. The limit is 64 bots, including the default one.

Messages from the same WeChat user to different bots stay in different sessions. Route a bot to another agent with a dispatch rule:

```json
{
  "agents": {
    "dispatch": {
      "rules": [
        {
          "name": "shop-bot",
          "agent": "shop",
          "when": { "channel": "weixin", "account": "shop" }
        }
      ]
    }
  }
}
```

### Configuration Fields

| Field | Description |
|---|---|
| `enabled` | Set to `true` to enable the channel at startup. |
| `token` | Access token for the default bot. |
| `account_id` | iLink bot id for the default bot. |
| `user_id` | iLink user id for the default bot. |
| `accounts` | Additional WeChat bots. Each one has its own token, sync cursor, and context tokens. |
| `accounts.<key>.allow_from` | Allow list for that bot only. Falls back to the channel `allow_from` when empty. |
| `allow_from` | (Optional) WeChat user IDs allowed to talk to the bots. If empty, anyone who can message the linked account can trigger the bot. |
| `proxy` | (Optional) HTTP proxy (for example `http://localhost:7890`). Extra bots inherit it when they do not set their own. |
| `bind_token` | Secret another service sends as `Authorization: Bearer <bind_token>` when it asks for a login QR code. |

## QR codes for another service

Another service can request a WeChat login QR from the gateway and display that image itself. PicoClaw stores the token after the scan and does not return it.

Put the secret in `~/.picoclaw/.security.yml`. The gateway listens on `127.0.0.1:18790` unless `gateway.host` and `gateway.port` say otherwise.

```yaml
channel_list:
  weixin:
    settings:
      bind_token: "replace-with-a-long-random-string"
```

```bash
curl -sS -X POST http://127.0.0.1:18790/api/weixin/binds \
  -H "Authorization: Bearer $BIND_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Zhang San","external_id":"user-42"}'
```

Omitting `account` creates a new bot, so that person's history stays separate. The response includes `qr_data_uri` and `qr_path`. Keep the bearer token on the calling service. Poll `GET /api/weixin/binds/{flow_id}` until `status` is `confirmed`. Status values are `wait`, `scanned`, `confirmed`, `expired`, and `error`. The QR expires in about five minutes. Pass `"account":"bot-2"` to reuse a bot, or `"channel":"name"` plus `?channel=name` on later requests to target another weixin channel. One channel holds at most 64 bots. A confirmed bind reloads the gateway so the new bot starts receiving messages.

## ⚠️ Important Notes

- **One token per bot**: each WeChat bot has its own iLink token. Linking the same WeChat bot twice is rejected. Scanning that bot's QR code somewhere else can invalidate the saved token.
- **Message Rate Limits**: To avoid getting your account restricted by WeChat anti-spam systems, avoid loop triggers or high-frequency broadcasts.
