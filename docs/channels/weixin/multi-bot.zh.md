# 微信多 Bot 改动说明

这次改动让 PicoClaw 的一个微信渠道可以挂多个微信 Bot。做法参照 [微伴 Wechatai](https://github.com/moutsea/wechatai) 的多条微信链接：每个 Bot 单独扫码、单独令牌、单独会话。原来的单 Bot 配置不用迁移。

## 实现了什么

- 一个 `type: weixin` 的渠道里可以有多个 Bot。默认 Bot 继续用原来的 `token`、`account_id`、`user_id`。其余 Bot 放在 `settings.accounts`。
- 每个 Bot 有自己的二维码登录、iLink 令牌、长轮询游标和 context token。同一个微信用户的聊天记录按 Bot 分开保存，重启后还在。
- 两个及以上 Bot 时，长轮询收成一个调度器，并共用一个 HTTP 连接池。微信若支持 HTTP/2，这些长轮询可以复用同一条连接。每个 Bot 仍有一次进行中的 `getupdates`，因为 iLink 按令牌鉴权，不能用一个令牌把所有 Bot 的消息拉下来。
- 命令行、Web 配置页都可以添加、切换和删除 Bot。扫码时若微信返回区域跳转（`scaned_but_redirect`），后续轮询会改到返回的主机。

## 修改了哪些地方

| 位置 | 改动 |
|---|---|
| `pkg/config/config.go` | 增加 `WeixinAccountSettings` 和 `settings.accounts`。令牌仍按原来的安全字段保存，不写进明文 `config.json`。 |
| `pkg/channels/weixin/accounts.go` | 账号列表、最多 64 个、重复令牌和重复 bot id 拒绝、添加与删除。 |
| `pkg/channels/weixin/multi.go` | 多个 Bot 合成一个渠道。入站消息带上 Bot 的 `account`，回复回到收到消息的那个 Bot。 |
| `pkg/channels/weixin/poller.go` | 共享轮询器。单个 Bot 时仍走原来的轮询循环。 |
| `pkg/channels/weixin/api.go` | 同一代理下的 Bot 共用 HTTP 连接池。 |
| `pkg/channels/weixin/weixin.go` | 入站消息写入 `account`，并带上 `weixin_account`。 |
| `cmd/picoclaw/internal/auth/weixin.go` | `picoclaw auth weixin` 增加 `--account`、`--name`、`--channel`，以及 `list`。 |
| `web/backend/api/weixin.go` | 增加创建和删除 Bot 的接口。扫码请求可以指定 Bot。 |
| `pkg/channels/weixin/bindapi.go` | 网关提供给别的服务的二维码接口。别的服务自己展示二维码，扫码成功后令牌写回 PicoClaw。 |
| `web/frontend` 微信配置页 | 切换 Bot、填写名称后添加、删除额外 Bot。默认 Bot 只能断开登录。 |
| 渠道文档 | `docs/channels/weixin/README.md` 与 `README.zh.md`，以及聊天应用指南里的多 Bot 说明。 |

会话键本来就会带上渠道名和 `account`，这次没有改会话系统。消息进网关之后，按 Bot 分开是原有分发规则加上这次写入的 `account`。

## 支持什么

一个微信渠道最多 **64** 个 Bot，包含默认 Bot。

| 项目 | 支持情况 |
|---|---|
| 一个 Bot | 一次扫码、一个微信账号、一份令牌。同一个微信号不能绑到两个 Bot。 |
| 每个人的记忆 | 按 Bot 分开，存在磁盘上。对话变长后，较早的内容收成摘要，仍留在这个人的会话里。 |
| 多个微信渠道 | 可以。`channel_list` 里写多条 `type: weixin`，每条自己最多 64 个 Bot。每条渠道至少有一个启用且已绑定（有 token）的 Bot 才会启动。 |
| 按人分发 | 消息里的 `from_user_id` 会写到 `sender`。`when.sender` 可以把不同微信用户交给不同 Agent。一个 Bot 就够。 |
| 按 Bot 分发 | `when.channel` 填渠道名字，`when.account` 填 Bot key，例如 `shop`。 |
| 别的服务展示二维码 | 可以。网关 `POST /api/weixin/binds`，请求头带 `Authorization: Bearer <bind_token>`。不指定 `account` 时每次新建一个 Bot。说明见 [微信渠道文档](README.zh.md)。 |
| 同时回复 | 默认 1 路。`agents.defaults.max_parallel_turns` 设为 3 就是同时 3 路。这是原有 Agent 配置，不是这次新加的接口。 |
| 空闲内存 | 网关大约 34MB。每多一个 Bot 大约 100KB。64 个 Bot 加上网关大约 42MB。128MB 全部给 PicoClaw、模型走云端时，空闲大约能挂 900 个 Bot；给正在进行的回复留余量，按大约 800 个。超过 64 个要再开渠道。 |

Bot 的 key 只能用字母、数字、`_` 和 `-`。显示名可以是中文，1 到 40 个字。没填代理或接口地址的额外 Bot，沿用渠道上的 `proxy` 和 `base_url`。`enabled: false` 的 Bot 不会轮询。

## 怎么用

每个人单独建一个 Bot，自己扫自己的码。谁扫码确认，这个 Bot 就绑到谁的微信上。

```bash
picoclaw auth weixin
picoclaw auth weixin --account shop --name 门店号
picoclaw auth weixin list
picoclaw gateway
```

指定另一条渠道：

```bash
picoclaw auth weixin --channel weixin_shop --account shop --name 门店号
```

手机路径：设置 → 插件（部分版本在「功能」）→ 微信 ClawBot → 扫一扫。

Web 配置页可以切换、添加和删除 Bot。这个页面只操作名字为 `weixin` 的那一条渠道。

配置示例和字段说明见 [微信渠道文档](README.zh.md)。

## 做不到的事

- 不能用一个 `getupdates` 按 `from_user_id` 或 bot id 把所有 Bot 的消息拉下来。拉消息只认该 Bot 的令牌。
- 不能让很多人扫同一个二维码。一次确认只绑定扫码的那个微信。
- 128MB 上不能跑本机大模型。模型需要走云端接口。
