package main

// helpText is the full -h/--help output. It is written for both humans and
// coding agents: the 推荐流程 section spells out the list -> pick -> verify
// loop, and every flag documents its default and its -list row semantics.
const helpText = `videofetch — 网页视频/图片抓取下载器

给一个网页 URL，自动提取页面里的视频流（直链 mp4/webm/mov/avi、HLS m3u8）
和 <img> 图片附件并下载。支持断点续传、HLS AES-128 解密、防盗链
Referer/UA、HTTP/SOCKS5 代理，以及 lurl.cc / drrop.cc 密码解锁。

用法
  videofetch -url <页面URL> [flags]     # 提取并下载（默认下载全部行）
  videofetch -url <页面URL> -list       # 只列出发现的流/图片，不下载
  videofetch -serve [flags]             # 运行下载目录的 HTTP 管理服务
  videofetch -v                         # 打印 Go build 信息并退出

推荐流程（agent 按此执行）
  1. 先 -list 看清楚有哪些行：每行 = 一个播放器（或其一路变体）或一张图片，
     行号 1-based，下载命名与行号一一对应
  2. 直接下载（全部行）或用 -pick 选行号
  3. 校验结果：行数与落盘文件数一致；file 应识别为真实媒体（ISO Media 等）
     而不是错误页/零字节

示例
  videofetch -url https://example.com/post.html -list
  videofetch -url https://example.com/post.html              # 下载全部行
  videofetch -url https://example.com/post.html -pick 1,3-4  # 只下第 1、3、4 行
  videofetch -url https://example.com/post.html -pick 1 -quality h264
  videofetch -url https://example.com/post.html -images      # 同时提取 <img> 图片
  videofetch -url https://example.com/post.html -proxy socks5://127.0.0.1:1140
  videofetch -url https://example.com/post.html -referer https://example.com/ \
             -ua "Mozilla/5.0 (Windows NT 10.0; Win64; x64) ..."
  videofetch -url https://lurl.cc/h3Uvq -list -ex -pass 1113      # 密码解锁
  videofetch -url https://drrop.cc/v/0728148421 -list -ex -pass 6666

输出命名
  每次运行生成一个 5 位随机小写前缀，文件为 <前缀>.<行号>.<扩展名>，
  行号与 -list 的行号一一对应。图片保留自身扩展名（.jpeg 归一为 .jpg）；
  URL 无扩展名时默认 .mp4。文件生成在当前目录。

主要 flags
  -url        页面地址（也可作为第一个位置参数）
  -list       只列出发现的流，不下载。HLS 行显示 时长/分片数/分辨率
              （需请求播放列表），直链/图片行显示文件大小
  -pick       只下载指定行；1-based，与 -list 行号一致；支持 1,3 / 1-4 /
              2,5-6 混合写法
  -quality    best | lowest | h264 | h265。master 播放列表按带宽挑；
              同一播放器带 h264/h265 双路时 -list 只显示 h265（体积小），
              要 h264 需 -pick 行号 + -quality h264
  -images     同时提取 <img> 图片附件，作为 kind=image 的行参与 -list/下载。
              懒加载属性 data-src/ess-data/file/iyl-data 优先于 src（反广告
              占位图在 src 里也不会污染结果）；同域图片（logo/头像/表情）
              自动跳过；单张失败只打印警告并跳过，不中断整批。
              注意：-pick 引用图片行时，本次运行必须带 -images，
              否则图片行根本不会被提取，行号会越界
  -ex         常规提取不到流时，尝试站点解锁流程（当前支持 lurl.cc 密码门、
              drrop.cc 密码视频）。页面本来就没锁时自动退回普通提取，
              默认行为不变
  -pass       -ex 解锁流程用的密码。
              lurl.cc: 页内 JS 的 pp_a 反爬哈希在本地复算 → POST
              session.php → 带 session cookie 重抓页面。注意 lurl 对错误
              密码也回 SUCCESS，真实判据是解锁后页面有没有媒体，拿不到会
              报 "wrong password?"
              drrop.cc: POST /api/verify-password 换签名流地址（token 5
              分钟有效、支持 Range、不绑 cookie）；错密码直接报
              wrong_password
  -proxy      http://host:port 或 socks5://host:port；socks5h 同义（DNS 在
              代理端解析，可避开本地 DNS 污染）
  -referer    防盗链 Referer，默认页面自身 origin
  -ua         User-Agent，默认 Chrome 126 (macOS)
  -workers    HLS 分片下载并发数（默认 8）
  -timeout    HTTP 响应头超时秒数（默认 60）
  -serve      运行 HTTP 文件管理服务；-addr 监听地址（默认 127.0.0.1:8080，
              :8080 才对 LAN 开放）、-dir 服务目录（默认 .）
  -v          打印 Go build 信息并退出

行为细节
  - 断点续传：直链下载中断后按 Range 续传（最多 5 次重试）；服务器不支持
    Range 时从头重下
  - HLS：自动解析 master 多码率列表、AES-128 解密、并发下分片；系统有
    ffmpeg 时把 TS 转封装为 mp4，否则保留 TS
  - 图片批下载中某张 404/TLS 失败只跳过该张，其余照常
  - 需要登录才可见的媒体（受保护账号、敏感内容）抓不到属正常现象
`
