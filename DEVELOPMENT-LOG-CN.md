# 贝小果开发日志

## 0.1.0 — 2026-11-23

### 建立项目骨架
- 新建 Go 单体服务，静态网页通过 embed.FS 编译进二进制。
- 接入用户维护的 openhongguo Go SDK Fork，并固定依赖提交。
- 使用 SDK memstore 保存匿名设备档案，避免写 N1 TF 卡。
- 实现 health、发现、搜索、联想、热搜、新剧、榜单、详情和分集 API。
- 建立手机优先的响应式网页首页、导航、搜索、剧集卡片、详情弹窗和本地收藏。
- 增加 ARM64/AMD64 构建命令、GitHub Actions CI 与架构文档。
- 播放和电视接力尚未实现；网页明确显示播放器开发状态，不伪装成已可播放。

### 验证状态
- 已核对 SDK 的真实 Go 包路径、调用方法、模型字段以及当前 Fork 提交。
- 当前会话通过 GitHub API 写入源码；尚未在 N1 或真实浏览器完成运行验收。
- 后续 CI 必须通过 Go 编译与测试，才能将该提交视为构建验证完成。


## 0.1.0 — 播放链路推进（2026-11-24）

### 新增
- 接入 SDK ResolvePlayback 与 ContentKey，在 N1 内存中保存播放会话、上游 CDN 地址和 CENC 密钥。
- 新增同源播放解析 API：GET /api/v1/play/{episodeID}?quality=。
- 新增媒体 Range API：GET /api/v1/media/{token}，支持 GET/HEAD、单段 Range、开区间和后缀 Range。
- 上游视频按 Range 读取，CENC AES-CTR 按 MP4 样本区间原地解密后流式返回。
- 浏览器剧集详情页的分集按钮已连接原生 HTML5 video，画质选择由 SDK 实际返回的流决定。
- 上游 URL 强制 HTTPS、仅 443 端口、拒绝本地/私有/共享地址，禁用 HTTP 重定向；媒体令牌随机生成且不写日志。
- 限制同时媒体流数量、会话数量、会话寿命和内存缓冲；不缓存整集，不写磁盘。

### 尚待验证
- CI 编译和单元测试通过后，还需在 N1 与真实手机/桌面浏览器测试 CDN 是否稳定支持 Range、HEVC/H.264 浏览器解码和 CENC 实际文件结构。
- 当前仅支持单文件 MP4，不支持 HLS/DASH 或音视频分离。
- 真实网络播放仍需处理签名 URL 在长时间观看中到期后的自动刷新。


### CI 验证记录
- 最新提交 0642d97 的 GitHub Actions 运行 45215498080 全部通过：Go 测试、Node 脚本语法检查、Linux AMD64/ARM64 构建和产物上传。
- ARM64/AMD64 构建产物：beixiaoguo-0.1.0-linux（GitHub Actions Artifact）。
- 通过的是自动化构建与合成 MP4 测试，不代表已完成 N1 实机或真实 CDN 播放验收。


## 0.1.0 — 图片、页面缓存与 TV 接力（2026-11-24）

### 新增
- SDK 目录响应内封面/头像 URL 统一改写为 N1 同源 `/api/v1/images/{token}`，避免浏览器直连第三方 CDN。
- N1 图片代理使用随机 token、同 URL 稳定映射、公开地址校验、禁止重定向、单图 8 MiB 上限和 96 MiB RAM LRU 缓存；命中时返回 ETag / 12 小时浏览器缓存。
- N1 对 discover/search/hot-search/rank/new-releases/series/episodes/suggest 增加有界 RAM TTL 缓存，不缓存播放解析和媒体数据。
- 浏览器维护最多 12 个列表视图状态，来回切换恢复列表、分页游标及滚动位置；手动刷新强制更新。
- 播放器新增 0.5× 至 3× 倍速、上一集/下一集按钮、纵向滑动切集、自动连播和网页全屏/手动全屏降级。
- 所有浏览器统一提供自定义投屏按钮；N1 mDNS 查询 `_hongguotv._tcp.local.`，按 hongguoTV protocol v1 JSON-line TCP 协议传送剧集、集数和播放位置，并校验回执。
- iOS 原生视频 AirPlay 接收入口通过 `disableRemotePlayback` / `x-webkit-airplay=deny` 禁用，避免误把 AirPlay 当作本项目 LAN 投屏。

### 自动化验证待 CI
- 新增 imageproxy 的稳定 token、缓存命中和 URL 拒绝测试。
- 新增 mDNS DNS-SD PTR/SRV/TXT/A 记录解析与 TV 设备构造测试。
- 新增 API RAM cache TTL/过期测试。
- 仍需 N1 实机确认真实图片 CDN 响应、mDNS 组播以及电视端接收播放。
