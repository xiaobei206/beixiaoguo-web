# 贝小果（beixiaoguo-web）

面向家庭局域网的短剧 Web 客户端。后端使用 Go，前端使用原生 TypeScript/JavaScript、HTML 与 CSS；构建后由一个 Go 可执行文件提供网页和 API。

> 当前版本：**0.1.2**
>
> 本项目是非官方个人项目，与红果短剧及其运营方无关联。请遵守内容平台的服务条款与适用法律，仅访问有权访问的内容。

## 0.1.2

- 图片通过 N1 同源代理访问，支持安全跟随有限 CDN 重定向，并使用 RAM 缓存，避免手机直接访问第三方图片 CDN。
- 页面切换保留已访问列表、分页游标和滚动位置；N1 目录 API 使用有界 RAM 缓存。
- 播放器支持 0.5×–3× 倍速、上下滑动切换集数、上一集/下一集和自动连播。
- 增加 N1 mDNS 发现 hongguoTV，并通过现有 protocol v1 将当前剧集、集数和播放位置交接到电视。
- mDNS 发现会分阶段查询 PTR、SRV、TXT、A/AAAA 记录，不再依赖电视一次性返回完整记录集；图片代理允许最多 5 次经过校验的重定向。
- 这些功能以 CI 自动化测试与 ARM64/AMD64 编译验证；真实 N1、手机 CDN 图片及电视端接力仍需实机验收。

## 目标与架构

- 为手机、平板和电脑提供真正响应式的浏览体验。
- 复用 [openhongguo Go SDK](https://github.com/xiaobei206/openhongguo) 的目录、搜索、榜单、剧集和播放模型接口。
- 面向 N1 / iStoreOS / ARM64，以单一静态 Go 二进制部署。
- 静态网页通过 `embed.FS` 嵌入程序，不需要 Node.js 运行时或数据库。
- SDK 匿名设备档案、目录缓存、封面缓存、媒体会话均在进程内存；默认不写 TF 卡。
- 播放链路已接入 CENC AES-CTR 按需解密与 HTTP Range 转发，不做整片下载、不做 N1 视频转码。
- 电视接力由 N1 服务端处理 mDNS 和 TCP，浏览器只访问同源 HTTP API。

## N1 / iStoreOS 安装与更新

推荐使用仓库内的一键更新脚本。脚本从 GitHub Releases 下载与设备架构匹配的正式版，校验 SHA-256，在 `/tmp` 中替换程序并启动服务；不把程序或日志写入 TF 卡：

```sh
wget -qO /tmp/beixiaoguo-install.sh https://raw.githubusercontent.com/xiaobei206/beixiaoguo-web/main/scripts/install-n1.sh && sh /tmp/beixiaoguo-install.sh
```

默认监听 `0.0.0.0:8780`，家庭网络中访问 `http://N1_IP:8780`。日志位于 `/tmp/beixiaoguo.log`。脚本会在当前 shell 的子进程中运行；正常交互式 SSH 会话应在脚本结束后返回提示符，它不会替你打开一个新的 SSH 窗口。

## 本地开发

需要 Go 1.22 或更新版本：

```sh
go run ./cmd/beixiaoguo
make test
make build-arm64
```

默认本机打开 `http://127.0.0.1:8780`。构建版本从仓库根目录的 `VERSION` 读取，并通过 linker flags 注入二进制。

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `BEIXIAOGUO_ADDR` | `0.0.0.0:8780` | HTTP 监听地址 |

## API

播放 API：`GET /api/v1/play/{episodeID}?quality=720p` 返回同源媒体地址；浏览器随后使用 `GET /api/v1/media/{token}` 和标准 `Range` 请求播放。令牌、上游 URL 与 CENC 密钥仅保存在服务端内存。

封面代理：目录 API 的 `cover_url` / `avatar_url` 等字段会被改写为 `/api/v1/images/{token}`。N1 最多缓存约 96 MiB 图片到 RAM，缓存 12 小时；浏览器获得 12 小时 HTTP 缓存。目录 API 响应也在 N1 内按操作类型做有限 TTL 缓存。

## 文档

- [架构与技术决策](docs/architecture.md)
- [开发日志](DEVELOPMENT-LOG-CN.md)
- [交接索引](HANDOFF-CN.md)
- [正式版本与下载](https://github.com/xiaobei206/beixiaoguo-web/releases)

## 许可证

本项目采用 MIT License。Go SDK 由独立依赖提供，并保留其各自许可证与声明。
