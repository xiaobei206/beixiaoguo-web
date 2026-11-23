# 贝小果交接索引

| 文件 | 用途 |
|---|---|
| [README.md](README.md) | 项目定位、N1 安装和更新说明 |
| [docs/architecture.md](docs/architecture.md) | 架构、API、内存策略、CENC 与 TV 接力方案 |
| [DEVELOPMENT-LOG-CN.md](DEVELOPMENT-LOG-CN.md) | 累计开发日志 |
| [VERSION](VERSION) | 当前项目版本 |

## 当前状态

- **0.1.1**：封面同源代理 / RAM 缓存、列表状态恢复、目录 API RAM 缓存、播放器 3× 倍速 / 滑动切集 / 自动连播、N1 到 hongguoTV 局域网接力。
- 播放器 / CENC Range 代理：第一版已接通，待 N1 实机播放验收。
- 图片代理：第一版已接入，待 Android/iOS 实机确认不同 CDN 图片格式及访问策略。
- 页面切换缓存、播放器倍速/切集/自动连播：第一版已接入，待实机验收。
- hongguoTV mDNS / protocol v1 一次性交接：Go 侧已接入，待同网段电视实机验收。
- HLS/DASH 与播放地址过期后的自动刷新：未实现。
- N1 实机运行和手机浏览器端到端验收：待完成。

## 发布流程

- 当前版本从根目录 VERSION 读取。
- 修改 VERSION 后，GitHub Actions Release 工作流会测试、交叉编译、创建 vX.Y.Z 标签并发布正式 Release。
- 正式 Release 附 ARM64、AMD64 二进制及 SHA256SUMS；N1 更新脚本通过 SHA-256 校验后部署到 /tmp。
- CI artifact 只用于临时构建验证，不再作为正式安装入口。

## 最近自动化验证

- 最近一次确认通过的 CI：commit 6bdd2a6，run 45222436061。
- 0.1.1 的新 Release 工作流会在 VERSION 更新提交进入 main 后自动运行；只有 GitHub Release 页面出现 v0.1.1 和对应附件，才算发布完成。
