# 贝小果（beixiaoguo-web）

面向家庭局域网的短剧 Web 客户端。后端使用 Go，前端使用原生 TypeScript/JavaScript、HTML 与 CSS；构建后由一个 Go 可执行文件提供网页和 API。

> 当前版本：0.1.0（开发中）
>
> 本项目是非官方个人项目，与红果短剧及其运营方无关联。请遵守内容平台的服务条款与适用法律，仅访问有权访问的内容。

## 目标

- 为手机、平板和电脑提供真正响应式的浏览体验。
- 复用 [openhongguo Go SDK](https://github.com/xiaobei206/openhongguo) 的目录、搜索、榜单、剧集和播放模型接口。
- 面向 N1 / iStoreOS / ARM64，以单一静态 Go 二进制部署。
- 静态网页通过 `embed.FS` 嵌入程序，不需要 Node.js 运行时或数据库。
- 默认把 SDK 匿名设备档案保存在进程内存，不写 TF 卡；服务重启后需要重新注册匿名设备。
- 播放链路目标是 CENC AES-CTR 按需解密与 HTTP Range 转发，不做整片下载、不做 N1 视频转码。
- 通过 N1 后端发现局域网内 hongguoTV，并复用现有一次性交接协议。

## 开发状态

第一阶段先建立稳定的服务端、浏览目录 API、响应式网页和 ARM64 构建。播放器、CENC Range 代理、电视发现/接力会在接口和协议核对完成后分阶段接入；未完成的能力不会在 UI 中伪装成可用功能。

## 本地运行

需要 Go 1.22 或更新版本：

```sh
go run ./cmd/beixiaoguo
```

浏览器打开 `http://127.0.0.1:8780`。

## 构建 N1 / iStoreOS

```sh
make build-arm64
```

输出：`dist/beixiaoguo-linux-arm64`。

在 N1 上可直接运行：

```sh
chmod +x beixiaoguo-linux-arm64
./beixiaoguo-linux-arm64
```

默认监听 `0.0.0.0:8780`。家庭网络中访问 `http://N1_IP:8780`。

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `BEIXIAOGUO_ADDR` | `0.0.0.0:8780` | HTTP 监听地址 |
| `BEIXIAOGUO_VERSION` | 构建注入 | 版本信息 |

## 文档

- [架构与技术决策](docs/architecture.md)
- [开发日志](DEVELOPMENT-LOG-CN.md)
- [交接索引](HANDOFF-CN.md)

## 许可证

本项目采用 MIT License。Go SDK 由独立依赖提供，并保留其各自许可证与声明。
