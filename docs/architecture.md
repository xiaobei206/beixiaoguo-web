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
       ├─ CENC Range 媒体代理（后续阶段）
       └─ mDNS / hongguoTV 接力（后续阶段）
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

前端收藏保存在当前浏览器的 localStorage，不上传到 N1；不同设备的收藏暂不互通。

## SDK

依赖固定到 \`xiaobei206/openhongguo\` Fork 的提交 \`bc620c4678c1\`。Go module 内部声明路径仍为 \`github.com/beilunyang/openhongguo\`，因此使用 go.mod replace 指向该 Fork。

## 视频播放计划（尚未实现）

目标链路：

1. Go 服务端调用 \`ResolvePlayback\` 取得播放模型，并用 \`ContentKey\` 在服务端取得 CENC 内容密钥。
2. 仅接受经过校验的上游 HTTPS 媒体地址，处理 Range 与上游响应头。
3. 按需读取有限范围的 MP4 字节，结合 MP4 样本表执行 AES-CTR 区间解密。
4. 向浏览器返回正确的 Range / Content-Range / Content-Length。
5. seek 时按需重新读取，不下载整集，不把内容密钥返回浏览器。

不能简单地对任意 HTTP Range 字节直接 AES 解密：CENC 需要正确解析 MP4 box、sample offset 与 subsample encryption 信息。正式实现前必须用已知测试样本验证随机 seek、跨 sample Range、最后一个字节以及并发读取。

第一阶段不做 FFmpeg 转码。浏览器硬件解码兼容性按设备测试；不能播放的编码格式后续再评估可选方案。

## 电视接力计划（尚未实现）

复用 \`hongguoTV\` 已有的 \`_hongguotv._tcp.\` DNS-SD 服务与 protocol v1 JSON-line TCP 一次性交接协议。浏览器不直接使用 raw TCP 或 mDNS；由 N1 Go 服务端负责发现和发送指令，浏览器只调用同源 HTTP API。若局域网组播不可用，再提供手工 IP/端口备用入口。

## 部署

N1 默认监听 \`0.0.0.0:8780\`，通过 \`BEIXIAOGUO_ADDR\` 修改。运行时不需要写入项目目录；若设备重启，进程内匿名档案会丢失并重新注册。
