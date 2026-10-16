# 💬 微信个人号渠道 (Weixin)

PicoClaw 支持使用腾讯官方 iLink API 连接微信个人号。同一个微信渠道可以挂多个 Bot：每个 Bot 单独扫码、单独长轮询，会话按 Bot 分开。做法与 [微伴 Wechatai](https://github.com/moutsea/wechatai) 的多条微信链接相同。

改动范围、实现方式和支持上限见 [多 Bot 改动说明](multi-bot.zh.md)。

面向多个独立用户提供服务时，见 [用户数据与工具隔离](isolation.md)。外部扫码绑定成功后会自动开启该渠道的隔离模式；已有渠道可配置 `settings.isolate_accounts: true`。

## 🚀 快速激活

最简单的方法是使用交互式 onboarding 命令进行一键激活：

```bash
picoclaw auth weixin
```

再添加一个 Bot：

```bash
picoclaw auth weixin --account shop --name 门店号
picoclaw auth weixin list
```

该命令将：
1. 从 iLink API 获取二维码并在终端中打印。
2. 等待你在手机微信中打开 **设置 → 插件（部分版本在「功能」）→ 微信 ClawBot → 扫一扫**。
3. 扫码确认后，把这个 Bot 的 token 写到对应账号。`--account` 为空时更新默认 Bot，填写新的 key 时新增一个 Bot。

配置完成后，即可启动网关：

```bash
picoclaw gateway
```

---

## ⚙️ 配置说明

您也可以在 `config.json` 的 `channels.weixin` 段目下进行手动维护。

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
        "proxy": "",
        "accounts": {
          "shop": {
            "name": "门店号",
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

默认 Bot 继续使用 `settings` 上原有的 `token` / `account_id`，旧配置不用改。其余 Bot 放在 `settings.accounts`，key 使用字母、数字、`_` 或 `-`。多个 Bot 共用一个轮询器和一个 HTTP 连接池。微信若支持 HTTP/2，这些长轮询会复用同一条连接；每个 Bot 仍有自己的 token 和一次进行中的 getupdates。上限是 64 个，包含默认 Bot。

同一个微信用户发给不同 Bot 的消息会进入不同会话。需要把某个 Bot 交给另一个 Agent 时，按账号分发：

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

### 字段解析

| 字段 | 说明 |
|---|---|
| `enabled` | 设置为 `true` 以在启动时激活该频道。 |
| `token` | 默认 Bot 的扫码令牌。 |
| `account_id` | 默认 Bot 的 iLink bot id。 |
| `user_id` | 默认 Bot 的 iLink user id。 |
| `accounts` | 额外的微信 Bot。每个 Bot 有自己的 token、游标和 context token。 |
| `accounts.<key>.allow_from` | 只作用于这一个 Bot。留空时沿用渠道的 `allow_from`。 |
| `allow_from` | (可选) 允许与机器人交互的微信 User ID 列表。如果为空，任何能给此微信号发消息的人都可以触发机器人。 |
| `proxy` | (可选) HTTP 代理地址（例如 `http://localhost:7890`）。额外 Bot 没有自己的 `proxy` 时沿用这里。 |
| `bind_token` | 给别的服务调用的绑定密钥。请求头带 `Authorization: Bearer <bind_token>`。 |

## 给别的服务展示二维码

别的服务不要打开 PicoClaw 网页。它请求网关，拿到二维码后在自己的页面里展示。扫码成功后，令牌只留在 PicoClaw，响应里不会带出微信令牌。

先在 `~/.picoclaw/.security.yml` 写入绑定密钥。网关需要能被这个服务访问到，默认是 `127.0.0.1:18790`。

```yaml
channel_list:
  weixin:
    settings:
      bind_token: "换成一段足够长的随机字符串"
```

生成二维码。不填 `account` 时，每次都会新建一个 Bot，这个人的聊天记录单独保存。

```bash
curl -sS -X POST http://127.0.0.1:18790/api/weixin/binds \
  -H "Authorization: Bearer $BIND_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"张三","external_id":"user-42"}'
```

返回里有 `qr_data_uri` 和 `qr_path`。别的服务用其中一种把图显示出来。`qr_path` 仍然要带同一个 `Authorization` 头，不要把绑定密钥交给访客的浏览器。

```bash
curl -sS "http://127.0.0.1:18790/api/weixin/binds/$FLOW_ID/qr.png" \
  -H "Authorization: Bearer $BIND_TOKEN" \
  -o qr.png
```

然后轮询，直到 `status` 变成 `confirmed`。二维码大约 5 分钟有效。

```bash
curl -sS "http://127.0.0.1:18790/api/weixin/binds/$FLOW_ID" \
  -H "Authorization: Bearer $BIND_TOKEN"
```

| 字段 | 含义 |
|---|---|
| `status` | `wait` 等待扫码，`scanned` 已扫码，`confirmed` 已绑定，`expired` 二维码过期，`error` 失败。 |
| `account` | PicoClaw 里的 Bot key，例如 `bot-2`。会话按这个 key 分开。 |
| `weixin_bot_id` | 绑定成功后的微信 Bot id。 |
| `external_id` | 你传进来的外部用户编号，原样返回，方便和你自己的用户对应。 |

指定已经存在的 Bot 时，在请求里加 `"account":"bot-2"`。要绑到另一条微信渠道时，加 `"channel":"渠道名"`，查询状态和图片时带上同样的 `?channel=渠道名`。一条渠道最多 64 个 Bot。确认绑定后网关会重新加载配置，新 Bot 开始收消息。

## ⚠️ 注意事项

- **一个 Bot 一份令牌**: 每个微信 Bot 的 iLink 令牌独立。把同一个微信号重复绑到两个 Bot 会被拒绝。在别处重新扫码可能让旧令牌失效。
- **频率控制**: 为避免触发微信的风控反垃圾机制，请避免设置死循环触发、高频广播等恶意行为。
