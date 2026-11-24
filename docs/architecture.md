# 贝小果架构与技术决策

## 总体结构

\`\`\`text
手机 / 平板 / 电脑浏览器
          │ 同源 HTTP
          ▼
     Go 单体服务（N1）
       ├─ embed.FS 静态站点
       ├─ REST API
       ├─ openhongguo Go SDK
       ├─ CENC Range 媒体代理
       ├─ 同源图片代理 / RAM + 磁盘两层缓存
       └─ mDNS / hongguoTV 接力
          │
          ├──────── 红果接口 / CDN
          └──────── 家庭 LAN 内 hongguoTV
\`\`\`

## 运行原则

- Go 服务和静态页面打包为一个 Linux ARM64 可执行文件。
- 浏览器与 API 同源，不启用宽泛 CORS，不需要 Node.js 运行时。
- 默认使用 SDK 的 \`memstore\`，匿名设备档案只存在进程内存。
- 不创建数据库、不写播放缓存、不落地完整媒体文件。
- 访问日志输出到标准输出；不在应用内部创建日志文件。
- 上游 API 最多 4 个并发请求，另有 24 个有界等待名额；25 秒总时限覆盖排队和上游调用。队列满时立即返回 503 和 Retry-After，浏览器最多自动重试一次；切换页面时取消过期请求。
- 图片下载独立于片库 API：最多 8 个并发下载、64 个等待名额；相同图片只允许一个源站下载任务，队列满时快速拒绝，不让无限请求占用内存。

## 当前 API

| 方法 | 路径 | 功能 |
|---|---|---|
| GET | \`/api/v1/health\` | 服务状态与版本 |
| GET | \`/api/v1/discover\` | 推荐发现列表、游标分页 |
| GET | \`/api/v1/search?q=\` | 短剧搜索 |
| GET | \`/api/v1/suggest?q=\` | 搜索联想 |
| GET | \`/api/v1/hot-search\` | 猜你想搜与热榜 |
| GET | \`/api/v1/new-releases\` | 新剧上线日历 |
| GET | \`/api/v1/rank\` | 短剧排行榜 |
| GET | \`/api/v1/series/{id}\` | 短剧详情 |
| GET | \`/api/v1/series/{id}/episodes\` | 分集列表 |
| GET | `/api/v1/series/{id}/seasons` | 多季合集及季列表 |
| GET | `/api/v1/play/{episodeID}?quality=` | 解析播放模型并创建短时媒体会话 |
| GET / HEAD | `/api/v1/media/{token}` | HTTP Range 媒体代理与 CENC 按需解密 |
| GET | `/api/v1/images/{token}` | 同源封面代理、RAM 缓存和 ETag |
| GET | `/api/v1/tv/devices` | mDNS 搜索 hongguoTV protocol v1 设备 |
| GET / PUT | `/api/v1/settings` | 运行时缓存与预热设置、状态和统计 |
| POST | `/api/v1/preheat/run` | 手动触发一次后台预热 |
| POST | `/api/v1/playback/activity` | 浏览器播放心跳，协调后台预热 |
| POST | `/api/v1/tv/handoff` | 将剧集、集数和播放位置交给指定电视 |

多季详情通过 `/api/v1/series/{id}/seasons` 调用 SDK `Seasons` 取得合集全部季；季列表独立异步加载，当前季高亮，有剧集的其他季可切换到对应详情，无可用剧集的季置灰。季数据在进程 RAM 中缓存 20 分钟。

前端收藏保存在当前浏览器的 localStorage，不上传到 N1；不同设备的收藏暂不互通。列表/详情/分集文字数据只放在 N1 RAM API 缓存，不建立永久剧情数据库；服务重启后由启动预热重新获取。页面过期时先尝试刷新，刷新失败仍可返回旧列表作为备用。浏览器页面切换保留每个列表、分页游标和滚动位置。

## SDK

依赖固定到 \`xiaobei206/openhongguo\` Fork 的提交 \`bc620c4678c1\`。Go module 内部声明路径仍为 \`github.com/beilunyang/openhongguo\`，因此使用 go.mod replace 指向该 Fork。

## 视频播放链路（第一版已接通）

当前链路：

1. Go 服务端调用 \`ResolvePlayback\` 取得播放模型，并用 \`ContentKey\` 在服务端取得 CENC 内容密钥。
2. 仅接受经过校验的上游 HTTPS 媒体地址，处理 Range 与上游响应头。
3. 按需读取有限范围的 MP4 字节，结合 MP4 样本表执行 AES-CTR 区间解密。
4. 向浏览器返回正确的 Range / Content-Range / Content-Length。
5. seek 时按需重新读取，不下载整集，不把内容密钥返回浏览器。

不能简单地对任意 HTTP Range 字节直接 AES 解密：CENC 需要正确解析 MP4 box、sample offset 与 subsample encryption 信息。Go CENC 包已覆盖 senc、subsample、saiz/saio、moov 位于 mdat 后、随机切片和并发读取的合成 MP4 测试；仍需使用真实授权测试样本验证实际 CDN 文件布局。

第一阶段不做 FFmpeg 转码。浏览器硬件解码兼容性按设备测试；不能播放的编码格式后续再评估可选方案。

## 电视接力（第一版已接通，待实机验收）

复用 \`hongguoTV\` 已有的 \`_hongguotv._tcp.\` DNS-SD 服务与 protocol v1 JSON-line TCP 一次性交接协议。浏览器不直接使用 raw TCP 或 mDNS；由 N1 Go 服务端查询 `_hongguotv._tcp.local.`，读取 TXT / SRV / A 记录，并通过 TCP 发送 JSON-line protocol v1。请求包含 `protocol=1`、`type=play`、`transferId`、`targetDeviceId`、`seriesId`、`episode` 和 `positionMs`；收到回执后校验 transferId 和 deviceId。浏览器只调用同源 HTTP API。若局域网组播不可用，再提供手工 IP/端口备用入口。

## 部署

N1 默认监听 \`0.0.0.0:8780\`，通过 \`BEIXIAOGUO_ADDR\` 修改。可执行文件固定安装在持久化路径 \`/usr/bin/beixiaoguo\`；\`/etc/init.d/beixiaoguo\` 使用 OpenWrt procd 开机启动，主进程以 \`respawn 3600 5 0\` 无限重试。另有健康检查实例每 15 秒访问本机 \`/api/v1/health\`，连续 3 次失败后向 procd 的 app 实例发送 TERM，以触发重启。日志写入 OpenWrt 的 RAM 日志缓冲，可通过 \`logread | grep -i beixiaoguo\` 查询。匿名档案、API RAM 缓存、图片 RAM 热缓存、播放会话和运行日志保存在内存；图片另有最多 1 GiB 的持久化文件缓存，重启后可继续命中。图片磁盘缓存目录默认为 `/opt/beixiaoguo/cache/images`，不预分配空间；空间不足或目录不可写时自动降级为 RAM 缓存。


## 列表分页与筛选标签

- 发现、搜索、排行榜和新剧页面第一次打开时主动逐页预载，目标至少 100 条；每页返回后立即追加显示，不等待整批完成。达到目标后，当滚动位置接近页面底部时再请求下一页，并保留手动加载按钮作为备用入口。
- 后端游标仍由 SDK 原样管理；新剧续页同时传递选中的日期。不同频道 / 搜索类型 / 排行榜类型与榜单 / 新剧日期使用独立页面缓存键。
- 每次切换栏目或筛选时，前端主动取消上一个列表视图的 fetch；同一视图的分页续载共享 AbortController，避免过期请求继续占用 N1 上游并发名额。
- 列表追加时按稳定剧集 ID 去重，只把新增卡片插入 DOM；如果服务端连续返回已见过的记录，则停止继续自动加载，避免无限请求。
- 筛选 TAG 对应真实查询参数：发现页 channel、搜索页 type、排行榜 content / board、新剧 date。搜索类型筛选只接受 SDK 已支持的真人剧、漫剧、AI 漫剧、影视剧与未看过枚举。
- 分页状态标签展示当前已载入数量、页数，以及首批预载进度 / 加载中 / 自动续载 / 已到底状态。加载失败不会触发定时自动重试，用户可主动刷新或点击加载更多。封面仍由浏览器按可视区域懒加载，避免大批图片请求阻塞卡片与名称显示。

## 图片、文字缓存与智能预热

- 图片 token 由 N1 随机生成；仅来自 SDK 目录/详情响应里的图片 URL 才会被注册，浏览器不传入任意目标 URL。
- 图片源只允许公开 HTTP/HTTPS 的 80/443 端口，解析 DNS 后拒绝私网、回环、链路本地和共享地址；最多跟随 5 次逐跳校验的 CDN 重定向，拒绝 HTTPS 降级。
- 图片单张上限 8 MiB。默认 RAM 图片热缓存 224 MiB，按闲置时间（默认 7 天）与容量 LRU 淘汰；RAM 命中不按 12 小时强制过期，只有闲置或容量淘汰后才从闪存恢复。
- 图片持久缓存默认上限 4 GiB，默认闲置保留 90 天，按 LRU 淘汰；设置可在网页运行时调整，写入前至少保留 1 GiB 系统剩余空间。文件以图片源 URL 哈希命名，原子写入，服务重启后可复用；仅成功回源的新图片产生磁盘写入，磁盘命中不触发重复写入。默认目录 `/opt/beixiaoguo/cache/images` 可通过 `BEIXIAOGUO_IMAGE_CACHE_DIR` 改到外接 SSD/USB 存储，不预分配。
- 图片读取顺序为 RAM → 磁盘 → CDN。RAM 图片闲置淘汰不会删除闪存副本；磁盘空间不足时按 LRU 淘汰旧图片。磁盘缓存保留时长和上限由 `/opt/beixiaoguo/settings.json` 的运行设置控制。
- 对 `fqnovelpic.com` / `byteimg.com` 的 `/novel-pic/` HEIC 封面，优先尝试官方 `p3-novel.byteimg.com` JPEG 变体；仅在 JPEG 文件头校验成功后缓存为 `image/jpeg`，变体请求失败或返回其他格式时记录脱敏诊断并回退原始图片。普通图片不转换，也不引入 FFmpeg。
- API 文字缓存默认 RAM 64 MiB、最多 4096 条。推荐/真人剧/漫剧/找剧/电影/小说列表默认 70 分钟，排行榜 40 分钟，搜索 6 小时，热搜 30 分钟，作品详情/季 24 小时，分集 12 小时，搜索联想 24 小时；各 TTL 会在网页端显示或按业务类型固定。缓存到期后仍保留旧值作为刷新失败时的备用响应，成功刷新才替换；没有大规模文字持久化文件，服务重启后重建。
- 设置文件只保存容量、保留时间、预热开关/数量和播放空闲阈值等少量配置；通过 `GET/PUT /api/v1/settings` 运行时读取/更新。网页设置无账号认证，按家庭局域网使用；输入值在服务端做范围限制，配置以原子文件替换保存。
- 开机自动预热与每小时整点巡检：六个发现频道（推荐、真人剧、漫剧、找剧、电影、小说）、当日新剧、排行榜各目标 100 条，热搜读取完整当前列表；实际数量以接口返回为准。只预热目录页与封面，不批量获取全部剧集详情/每部剧的所有分集，以避免无意义上游请求。
- 后台目录/API 预热 1 个工作槽，每个请求独立 10 秒超时，所有请求至少间隔 1.2 秒；图片预热最多 2 路、每个请求 15 秒超时。失败/超时单项记录并跳过，不会阻塞后续频道；整轮不并发重叠。前台 API 请求队列有等待任务时不启动新的后台调用，后台最多占用 1 个上游槽位，为前台保留其余 3 个。
- 播放活动由浏览器视频 playing/pause/ended/error 事件及 15 秒心跳报告；N1 也统计直接媒体 Range 流量，以覆盖电视接力。播放中取消后台预热，播放流结束或心跳停止后默认等待 3 分钟才恢复，阈值可调。失败计数、成功条目、图片任务进度、最后结果和下一次运行时间显示在设置页。
- 浏览器图片响应使用 7 天 HTTP 缓存，并声明 stale-while-revalidate / stale-if-error；浏览器可复用本机缓存，N1 的闪存图片缓存则跨设备、跨服务重启复用。
- `/api/v1/health` 额外报告 API 缓存、图片 RAM/闪存缓存占用，以及 API/图片队列活跃与等待数量；网页设置页每 5 秒刷新状态但不会覆盖当前编辑中的设置。
