# videofetch

通用网页视频抓取工具：输入一个网页 URL，自动分析页面里的播放器配置，提取视频流地址并下载。

支持：

- 直链视频（.mp4 / .webm / .flv 等）直接下载，支持断点续传
- HLS（.m3u8）分段流下载：自动解析 master 多码率列表、AES-128 解密、并发下载分片；支持 MPEG-TS（.ts）和 fMP4（`EXT-X-MAP` init 段 + 碎片）两种分片格式
- 常见播放器配置提取：`<video>/<source>` 标签、`og:video` meta、JSON-LD `VideoObject`、DPlayer 类 `data-config`（自动识别 h264/h265 双路流）、同域 iframe 播放页（跟进 2 层）
- HTTP / SOCKS5 代理（socks5h 由代理端解析 DNS，避免本地 DNS 污染）
- 自定义 Referer / User-Agent（防盗链）
- 系统有 ffmpeg 时自动把 TS 分片转封装为 mp4，否则保留原始 TS
- `-list` 展示每条流的时长 / 分片数 / 分辨率 / 文件大小，便于区分正片与广告、预览等短内容

## 安装

```sh
cd videofetch
go build -o videofetch .
```

## 用法

```sh
# 列出页面里发现的所有视频流（类型 / 来源 / 时长 / 分片数 / 大小）
# 行号从 1 开始，一个播放器一行；同一播放器有 h264 + h265 两路时只显示 h265（体积更小）
videofetch -url <页面URL> -list

# 下载全部（-list 显示的所有流，每播放器一路，默认 h265 变体；输出到当前目录，随机命名）
videofetch -url <页面URL>

# 只下载第 1 和第 3 行（对应 -list 的行号，1-based）
videofetch -url <页面URL> -pick 1,3

# 下载第 1 到第 4 行（范围写法）
videofetch -url <页面URL> -pick 1-4

# 指定代理（被墙站点必须用 socks5h 让代理端解析 DNS）
videofetch -url <页面URL> -proxy socks5://localhost:1140

# 防盗链：指定 Referer 与 UA
videofetch -url <页面URL> -referer https://example.com/ -ua "Mozilla/5.0 ..."

# -pick 选的是播放器，-quality 选编码：明确要所选播放器的 h264 变体
videofetch -url <页面URL> -pick 1,3 -quality h264

# 站点解锁：页面初始抓不到流时走站点解锁流程（支持 lurl.cc 密码门、drrop.cc 密码视频）
videofetch -url https://lurl.cc/h3Uvq -list -ex -pass 1113
videofetch -url https://drrop.cc/v/0728148421 -list -ex -pass 6666

# 完整参数
videofetch -url <页面URL> -proxy socks5://localhost:1140 \
           -pick 1-4 -quality best -workers 8 -timeout 60
```

输出命名：每次运行生成一个 5 位随机小写字母前缀，所有文件为 `<前缀>.<行号>.mp4`（如 `abcde.1.mp4`、`abcde.3.mp4`，行号对应 `-list` 的行号，一一对应）。文件生成在当前目录。

参数说明：

| 参数 | 默认 | 说明 |
|------|------|------|
| `-url` | 必填 | 页面地址（也可作为第一个位置参数） |
| `-pick` | 全部 | 只下载指定行；不指定时下载 `-list` 显示的所有流。1-based，与 `-list` 行号一致；支持 `1,3`、`1-4`、`2,5-6` 混合 |
| `-proxy` | 无 | `http://host:port` 或 `socks5://host:port`（socks5h 同义） |
| `-referer` | 页面自身 origin | 防盗链 Referer |
| `-ua` | Chrome UA | User-Agent |
| `-list` | false | 只列出发现的流，不下载；HLS 显示时长/分片数/分辨率（需请求播放列表），直链显示文件大小 |
| `-quality` | best | `best`/`lowest`：master 列表按带宽挑；`h264`/`h265`：与 `-pick` 组合时决定所选播放器用哪路变体。默认优先 h265（同画质体积约小一半，兼容性略差），明确要 h264 时用 `-quality h264` |
| `-workers` | 8 | HLS 分片并发数 |
| `-timeout` | 60 | HTTP 响应头超时（秒） |
| `-images` | false | 同时提取 `<img>` 图片附件：在 `-list` 里以 kind `image` 显示，按直链下载；懒加载属性（`data-src`/`ess-data`/`file` 等）优先于 `src`，同域图片（logo/头像/表情）自动跳过；单张失败只跳过不中断。`-pick` 行号同样适用 |
| `-ex` | false | 常规提取不到流时尝试站点解锁流程（支持 lurl.cc 密码门：算 pp_a 反爬哈希 → POST session.php → 带 cookie 重抓页面；drrop.cc 密码视频：POST verify-password 换签名流地址，token 5 分钟有效）。无锁页面自动退回普通提取，默认行为不变 |
| `-pass` | 无 | `-ex` 站点解锁用的密码；lurl.cc 对错误密码也回 SUCCESS，真实判据是解锁后页面有无媒体，拿不到会报 "wrong password?"；drrop 错误密码直接报 wrong_password |

## 局域网视频服务

`-serve` 把当前目录的视频做成一个网页列表，浏览器/手机可以直接播放、删除。

```sh
# 默认监听所有网卡（:8080），启动后日志会打印局域网可用的地址
videofetch -serve

# 指定端口和视频目录
videofetch -serve -addr :9000 -dir ~/Downloads

# 只允许本机访问（不要暴露到局域网）
videofetch -serve -addr 127.0.0.1:8080
```

启动日志里 `局域网访问: http://192.168.x.x:8080` 那几行就是给其他设备用的地址（优先列出私有网段 IPv4）。注意：能访问该地址的同网段设备也可以删除视频；公共 Wi-Fi 下建议用 `-addr 127.0.0.1:8080` 限制访问。如果局域网设备仍连不上，先检查 macOS 防火墙/本地网络权限是否放行 `videofetch`。

## 通用原理

1. **找真实媒体地址**：抓页面 HTML，从播放器配置里找出视频流。常见位置：`<video>/<source>`、`og:video` meta、JSON-LD `VideoObject`、DPlayer 等播放器的 `data-config` JSON、iframe 播放页。挖出来通常是两种：`.mp4` 直链，或 `.m3u8`（HLS）。
2. **直链**：直接 HTTP 下载，服务端支持 Range 时中途断线可续传。
3. **HLS**：解析 m3u8（含 `#EXT-X-STREAM-INF` 则是多码率 master，按带宽挑选一路）→ 并发下载 `.ts` 分片 → 按 `EXT-X-KEY` 的 key 和 IV 做 AES-128-CBC 解密（未指定 IV 时用分片序号）→ 按序拼接 → ffmpeg `-c copy` 转封装成 mp4。

常见问题：

- **auth_key 限时**：很多站点在页面/播放列表里动态签发带时效的签名 URL，过期后下载报 403，重新跑一次即可拿到新签名。
- **防盗链**：CDN 校验 Referer 时加 `-referer`。
- **被墙域名**：加 `-proxy socks5://...`，代理端解析 DNS 避免污染。
- **不支持的**：DRM（Widevine / FairPlay）、`SAMPLE-AES` 加密、`BYTERANGE` 分片（少见，遇到会明确报错而不是产出坏文件）。

仅用于你有权下载的内容。
