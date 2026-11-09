<p align="center">
  <img src="assets/logo.png" width="96" height="96" alt="OMC Qoder OAuth logo">
</p>

<h1 align="center">OMC Qoder OAuth</h1>

<p align="center">
  <a href="https://github.com/WizisCool/oh-my-cpa">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-dark.svg">
      <img src="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-light.svg" alt="Oh-My-CPA" height="22">
    </picture>
  </a>
</p>

<p align="center">
  专为 Oh-My-CPA 适配，兼容
  <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI</a>
  的原生 Qoder / Qoder CN OAuth 登录
</p>

<p align="center">
  <a href="https://github.com/omc-community/omc-qoder-oauth/releases"><img src="https://img.shields.io/github/v/release/omc-community/omc-qoder-oauth?label=release" alt="Release"></a>
  <a href="https://github.com/omc-community/omc-qoder-oauth/actions/workflows/build.yml"><img src="https://github.com/omc-community/omc-qoder-oauth/actions/workflows/build.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/CLIProxyAPI-v8+-4f46e5" alt="CLIProxyAPI v8+">
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license">
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a>
</p>

OMC Qoder OAuth 为 CLIProxyAPI 增加 `qoder` provider。在面板的 OAuth 流程里用
Qoder 或 Qoder CN 账号登录后，CPA 即获得基于 Qoder 自身目录的模型发现、OpenAI 兼容
执行和标准化配额，无需独立进程，也无需本地 Qoder 桌面客户端。

本项目是 [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa) 生态的一部分：
Oh-My-CPA 是 CLIProxyAPI 的自托管控制面板与用量观测台。

## 两个门户，一个 provider

Qoder 账号可以通过两个门户登录。两者产生同一种凭证（`"type": "qoder"`）并共用同一个
executor，但它们是不同主机上的不同账号，因此插件注册两个独立 OAuth 登录入口，而不是
一个入口加一个 `site` 下拉框。

| 门户 | 登录入口 | 登录页 | OpenAPI 主机 |
| --- | --- | --- | --- |
| Qoder | `qoder` | `https://qoder.com/device/selectAccounts` | `openapi.qoder.sh` |
| Qoder CN | `qoder-cn` | `https://qoder.cn/device/selectAccounts` | `openapi.qoder.com.cn` |

在渲染逐入口元数据的面板（CPA v8.0.51+）上，每个入口各自成为一张卡片，带自己的名称
和 logo。加载本插件需要 **CPA v8.0.49+**（schema 10 自该版本起提供），独立的名称与
图标需要 v8.0.51+。在更旧的宿主上两个标识符仍然可用，面板会为它们显示插件自身的名称
和 logo。账户级配额 `429` 会在 **CPA v8.0.62+** 上冷却整个凭证；更旧的宿主只冷却上报的模型。

- 选入口即选门户。入口优先于任何 `site` 提示。
- 每份凭证都会记录签发它的门户，字段为 `site`，同时写入凭证的 `oauth_provider`。
- `site`（默认 `qoder`）只是未指定入口时的兜底，例如宿主仅以插件标识符发起登录。
- 显式但无效的 `site` 值会被拒绝，而不是静默解析成默认值。

## 面板无关

所有面向用户的部分都由宿主渲染：OAuth 卡片、配置字段、静态资源和配额视图都通过 CPA
的插件 ABI 与管理 API 呈现。插件在
[Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa) 中是一等公民，在 CPA 自带的
Management Center 或任何对接 CPA 管理 API 的面板中表现一致。

OAuth 发起和轮询使用 CPA 原生受认证管理 API。公共插件资源仅包含静态 logo 与可选的
描述声明，即 `logo.png`、两个 SVG 与 `omc.json`。对这些路由的请求无法发起登录流程，查询参数也不会
触发任何动作。

登录使用 Qoder 自带桌面客户端的 PKCE 设备流程。插件构造入口的
`device/selectAccounts` URL，轮询门户的 OpenAPI 主机取得设备令牌，再用该令牌换取模型
调用使用的 job 令牌。无需本地回调监听器，也无需手动粘贴回调地址。

## 功能

- Qoder 与 Qoder CN OAuth，使用 PKCE 设备流程，每个门户一个登录入口。
- job 令牌交换，refresh 令牌单次使用；拒绝交换的 Qoder CN 账号保留设备令牌作为聊天令牌；
  job 令牌失效后，账号改用其设备令牌签名。
- 基于 Qoder `/algo/api/v2/model/list` 目录的模型发现，按目录中的模型名对外发布（`GLM-5.3`
  发布为 `glm-5.3`）；目录读取失败时使用内置兜底列表。内部 key 仍然可用，为 `gmodel`
  写的请求依旧解析到同一个模型。
- 以 OpenAI chat completions 收发的请求，支持流式与非流式，包括工具调用、推理内容和
  图片输入。
- 基于 Qoder `/sash/api/v2/me/usage` 用量信封的标准化配额。
- 按凭证的并发控制。
- 导入已有的 `"type": "qoder"` 认证文件。
- 错误映射保留各自的语义：排队提示、用量上限、免费模型的每日额度耗尽和登录失效各自返回
  自己的状态码，不会合并成笼统的上游失败。Qoder 在用量上限上给出 `Retry-After` 时，
  限流同时带上还剩多久，CPA 因此会让凭证休息，而不是在窗口内继续重试。

## 安装

将本仓库添加为插件商店源：

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-qoder-oauth:
      enabled: true
```

然后在插件商店安装 `omc-qoder-oauth`，并在面板中打开它的 OAuth 卡片。商店与共享库
名称为 `omc-qoder-oauth`；CPA 凭证 provider 标识保持为 `qoder`。登录、配置、配额与
故障排查见 [docs/USAGE.md](docs/USAGE.md)。

推送 `v*` 标签时由 GitHub Actions 创建发布。

## 配置

| 字段 | 类型 | 默认值 | 可选值 |
| --- | --- | --- | --- |
| `site` | 枚举 | `qoder` | 未指定入口时的兜底门户：`qoder`、`qoder-cn` |
| `max_concurrency` | 整数 | `3` | 每凭证 1–100 个并发请求 |
| `cosy_version` | 字符串 | `1.1.69` | COSY 信封声明的 Qoder 客户端版本 |
| `max_tokens` | 整数 | `32000` | 客户端未指定时的输出预算，1–1000000 |

## 开发

插件实现位于 [`go/`](go/)。模块下载完成后，测试无需网络：

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
```

为本机构建原生库：

```sh
cd go
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-qoder-oauth.so .
```

macOS 使用 `.dylib`，Windows 使用 `.dll`。`build/` 已被 git 忽略。

发布工作流从标签构建以下产物：

```text
omc-qoder-oauth_<version>_linux_amd64.zip
omc-qoder-oauth_<version>_linux_arm64.zip
omc-qoder-oauth_<version>_darwin_amd64.zip
omc-qoder-oauth_<version>_darwin_arm64.zip
omc-qoder-oauth_<version>_windows_amd64.zip
omc-qoder-oauth_<version>_windows_arm64.zip
checksums.txt
omc-qoder-oauth.omc.json
```

每个压缩包根目录内含命名正确的 `omc-qoder-oauth.so`、`omc-qoder-oauth.dylib` 或
`omc-qoder-oauth.dll`。`checksums.txt` 覆盖全部压缩包，插件商店会用它校验下载的压缩包。
`omc-qoder-oauth.omc.json` 是可选的描述声明，详见 [docs/USAGE.md](docs/USAGE.md)；
它发布的内容与插件内嵌并对外提供的字节完全一致。

提交规范、测试要求与生态开发习惯参见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 项目结构

```text
assets/logo.png         插件贴纸：插件在商店中的标识
assets/qoder-color.svg  官方浅色底标识，用于 Qoder 入口
assets/qoder.svg        反色标识，用于 Qoder CN 入口
go/                     Go/cgo 插件实现与测试
go/assets/              内嵌图稿与 omc.json 声明
docs/USAGE.md           部署安装与使用指南
```

Go 实现独立于面板。COSY 信封、设备流程等协议来源记录在 [NOTICE.md](NOTICE.md)。

## 许可证

[MIT](LICENSE)
