# 贝小果交接索引

| 文件 | 用途 |
|---|---|
| [README.md](README.md) | 项目定位、运行和 ARM64 构建说明 |
| [docs/architecture.md](docs/architecture.md) | 架构、API、内存策略、CENC 与 TV 接力方案 |
| [DEVELOPMENT-LOG-CN.md](DEVELOPMENT-LOG-CN.md) | 累计开发日志 |
| [VERSION](VERSION) | 当前项目版本 |

## 当前状态

- 0.1.0：Go 服务骨架、片库浏览 API、响应式网页已建立。
- 播放器 / CENC Range 代理：第一版已接通，待 N1 实机播放验收。
- hongguoTV 发现与一次性交接：未实现。
- HLS/DASH 与播放地址过期后的自动刷新：未实现。
- N1 实机运行和手机浏览器验收：待完成。

### 最近验证
- GitHub Actions：Go tests / Node syntax / Linux AMD64 / Linux ARM64 全部通过。
- 构建包：beixiaoguo-0.1.0-linux，见 [Actions run 45215498080](https://github.com/xiaobei206/beixiaoguo-web/actions/runs/45215498080)。
