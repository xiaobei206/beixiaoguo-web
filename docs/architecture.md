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
       ├─ 同源图片代理 / RAM 缓存
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
- 上游 API 并发限制为 4 个请求，单次操作超时 25 秒。

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
| GET | `/api/v1/play/{episodeID}?quality=` | 解析播放模型并创建短时媒体会话 |
| GET / HEAD | `/api/v1/media/{token}` | HTTP Range 媒体代理与 CENC 按需解密 |
| GET | `/api/v1/images/{token}` | 同源封面代理、RAM 缓存和 ETag |
| GET | `/api/v1/tv/devices` | mDNS 搜索 hongguoTV protocol v1 设备 |
| POST | `/api/v1/tv/handoff` | 将剧集、集数和播放位置交给指定电视 |

前端收藏保存在当前浏览器的 localStorage，不上传到 N1；不同设备的收藏暂不互通。目录 API 在 N1 RAM 内做有限 TTL 缓存，浏览器则在页面切换时保留每个列表、分页游标和滚动位置。

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

N1 默认监听 \`0.0.0.0:8780\`，通过 \`BEIXIAOGUO_ADDR\` 修改。运行时不需要写入项目目录；若设备重启，进程内匿名档案会丢失并重新注册。


## 列表分页与筛选标签

- 发现、搜索、排行榜和新剧页面第一次打开时主动逐页预载，目标至少 100 条；每页返回后立即追加显示，不等待整批完成。达到目标后，当滚动位置接近页面底部时再请求下一页，并保留手动加载按钮作为备用入口。
- 后端游标仍由 SDK 原样管理；新剧续页同时传递选中的日期。不同频道 / 搜索类型 / 排行榜类型与榜单 / 新剧日期使用独立页面缓存键。
- 列表追加时按稳定剧集 ID 去重，只把新增卡片插入 DOM；如果服务端连续返回已见过的记录，则停止继续自动加载，避免无限请求。
- 筛选 TAG 对应真实查询参数：发现页 channel、搜索页 type、排行榜 content / board、新剧 date。搜索类型筛选只接受 SDK 已支持的真人剧、漫剧、AI 漫剧、影视剧与未看过枚举。
- 分页状态标签展示当前已载入数量、页数，以及首批预载进度 / 加载中 / 自动续载 / 已到底状态。加载失败不会触发定时自动重试，用户可主动刷新或点击加载更多。封面仍由浏览器按可视区域懒加载，避免大批图片请求阻塞卡片与名称显示。

## 图片和目录缓存

- 图片 token 由 N1 随机生成；仅来自 SDK 目录/详情响应里的图片 URL 才会被注册，浏览器不传入任意目标 URL。
- 图片源只允许公开 HTTP/HTTPS 的 80/443 端口，解析 DNS 后拒绝私网、回环、链路本地和共享地址；禁用重定向。
- N1 图片缓存上限约 96 MiB，单张最多 8 MiB，缓存 12 小时；内存压力时按最近使用情况淘汰。缓存不写入 TF 卡。
- 对 `fqnovelpic.com` / `byteimg.com` 的 `/novel-pic/` HEIC 封面，优先尝试官方 `p3-novel.byteimg.com` JPEG 变体；仅在 JPEG 文件头校验成功后缓存为 `image/jpeg`，变体请求失败或返回其他格式时记录脱敏诊断并回退原始图片。普通图片不转换，也不引入 FFmpeg。
- 列表/搜索/榜单/详情 API 的结果按 operation 做 RAM TTL 缓存，最多 192 项、16 MiB；播放解析不缓存。
- 浏览器页面切换时恢复已访问页面的列表、分页游标和滚动位置；刷新按钮会强制重新请求。
