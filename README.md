# V. Adapter — NovelAI 协议适配服务（后端对接 OpenAI 兼容生图 API）

对任何支持 NovelAI 协议的客户端（酒馆 / SillyTavern 等前端），把它的 **NovelAI 渠道**接到
上游生图 API（OpenAI 兼容）上：本服务"假装自己是 NovelAI 官网"，接收 NovelAI 协议请求 →
翻译成 OpenAI 兼容请求 → 把生成的图打包成 ZIP 按 NovelAI 格式返回。

> **客户端一行代码都不改**，只在它的 NovelAI 渠道面板里把 URL 填成本服务地址即可。

## 快速开始

```bash
# 本地开发（Windows）
go build -o v-adapter.exe .
./v-adapter.exe

# 交叉编译 Linux 服务器单二进制
# Windows: 双击 build-linux.bat   或:  $env:GOOS="linux"; $env:GOARCH="amd64"; go build -o v-adapter .
```

> 启动时终端会打印绿色像素横幅（V. ADAPTER + 配置信息框）；重定向到文件时自动变纯文本。

启动后：

| 地址 | 用途 |
|---|---|
| `http://127.0.0.1:8888/` | 管理面板（运行总览 / 设置中心 / 生成记录） |
| `http://<服务器IP>:8888` | 填进客户端 NovelAI 渠道的 URL（**不要**带 `/ai`，客户端会自动补） |

## 客户端侧配置（NovelAI 渠道）

- **URL**：`http://<服务器IP>:8888`
- **Key**：服务端的 `nai_key`（默认 `v-adapter-8888`，可在面板「设置中心」或 `config.json` 修改；清空则任意 key 均可）
- **模型**：随便选（如 `nai-diffusion-5-full`），服务端忽略模型名
- **分辨率**：客户端面板值会原样转成 `宽x高` 发给上游；上游拒绝时自动回退到 `default_size`
- **vibe 参考图**：不支持（`/ai/encode-vibe` 返回 404），请勿启用

## 配置

`config.json` 是启动默认值，会被环境变量覆盖，再被面板保存的 `data/settings.json` 覆盖
（面板改动最高优先级，保证热生效）：

```json
{
  "listen": "0.0.0.0:8888",
  "qwen_url": "http://127.0.0.1:4000/v1",
  "qwen_key": "sk-你的上游密钥",
  "qwen_model": "qwen3.8-max",
  "default_size": "1024x1024",
  "nai_key": "v-adapter-8888",
  "chat_fallback": "auto"
}
```

环境变量（非空即生效，优先级高于 `config.json`、低于面板设置）：
`VADAPTER_QWEN_URL`/`OPENAI_BASE_URL`、`VADAPTER_QWEN_KEY`/`OPENAI_API_KEY`、
`VADAPTER_QWEN_MODEL`/`OPENAI_IMAGE_MODEL`、`VADAPTER_LISTEN`、`VADAPTER_DEFAULT_SIZE`、`VADAPTER_NAI_KEY`。

## 端点

| 端点 | 说明 |
|---|---|
| `POST /ai/generate-image` | 生图。请求 NovelAI 格式 JSON，响应 **ZIP**（内含 `image_0.png`） |
| `GET /ai/user/subscription` | 测试连接，返回 `{"tier":0,"active":true,...}`（客户端显示「连接正常:Free」） |
| `POST /ai/encode-vibe` | 不支持，404（客户端提示 vibe 编码失败） |
| `GET /` | 管理面板 |
| `GET /health` | 健康检查 |
| `GET /admin/status` | 运行状态 + 脱敏配置 + 最近记录（公开） |
| `GET/POST /admin/settings` | 读/写设置（需登录；GET 不返回明文 key） |
| `POST /admin/test` | 实调一次生图（需登录） |
| `POST /admin/translate` | 角色转译，可**同时出图**（需登录，见下节） |
| `GET /admin/styles` | 画风预设列表与当前选择（需登录） |
| `GET/DELETE /admin/logs` | 生成记录（需登录） |
| `/admin/auth/status\|login\|setup\|logout` | 面板登录（首次免密、可设密码） |

## 核心能力

1. **OpenAI 兼容调用三件套**：优先 `b64_json` 解码 → 无 b64 取 `url` 下载 → 带 `negative_prompt`
   失败自动去掉该字段重试一次；
2. **非图片内容识别**：返回 HTML/风控页时给出人话错误（"API 返回的不是图片，而是…"），
   不把乱码当图，且 auto 模式下自动转**聊天接口生图兜底**（含 502/429/超时重试、
   punish 验证页识别、内容安全策略提示）；
3. **尺寸格式**：`"宽x高"` 小写 x（如 `832x1216`），上游拒绝尺寸时回退默认尺寸重试；
4. **配置解析**：config.json + 环境变量；
5. **超时**：出图请求 300s、下载 180s、聊天 320s；不可用时返回人话错误（客户端会展示 message）。

### 实测：这个上游生图 API 的现状

- `POST {qwen_url}/images/generations` **稳定 500**：`Cannot access 'upstreamStream' before initialization`
  （上游自身故障，与尺寸/参数无关）；
- `POST {qwen_url}/chat/completions` **可用**，能正常返回 Markdown 图片链接。

因此 **auto 模式下实际出图走的是「聊天接口兜底」**（先试标准接口，约 0.3s 失败后自动转聊天，
单张约 30~60s）。若想省掉这次必然失败的尝试，可在面板把 `chat_fallback` 设为 `chat_only`。

兜底触发条件（不只风控页，接口级报错也转）：
拿到非图片内容 / 上游回风控验证码 / 上游返回 4xx（400/404/429 等）或 5xx → 转聊天；
**401/402/403 不转**（鉴权类错误聊天同样会失败，直接报错更快）。

### 交付包说明

- `data/settings.json`、`data/auth.json` 首次在面板保存设置 / 首次设置密码时自动创建，**交付包中不含**；
  未创建前以 `config.json`（+环境变量）为准。
- 已附带编译好的 `v-adapter`（Linux amd64，上传服务器 `chmod +x` 后运行）与 `v-adapter.exe`（Windows 本地/测试）。

## 不包含的功能

Gradio 界面、素材库、15 种生成模式、像素后处理 `pixel_forge`、变体树、Python 依赖 —— 全部不保留。

## 管理面板

- **运行总览**：版本/运行时长/成功失败计数、监听地址、脱敏配置、客户端对接信息、测试连接（带预览图）、最近记录
- **画风预设**：14 个内置动漫/插画画风 + 自定义；选中后**客户端发来的所有生图请求自动套用**
- **角色转译**：**输入一句描述直接出图**。转译出的提示词（总提示词/角色描述/场景与画风/负面词/推荐尺寸）
  一并展示供参考复制；可自选尺寸、可选是否套用面板画风预设；出图后可「再出一张」（沿用同提示词，只换种子）
- **生成记录**：最近 200 条（时间/类型/尺寸/链路/耗时/结果/提示词/错误）
- **设置中心**：qwen_url / qwen_key / qwen_model / default_size / nai_key / chat_fallback / listen 热改并持久化；
  面板密码设置/修改/关闭
- **安全**：Key 全程脱敏（面板与 `/admin/settings` 都不返回明文）；面板可设密码（会话 cookie 24h，存 `data/auth.json`）

## 角色转译出图

面板「角色转译」= 描述 → 出图，两步在一次请求里完成：

```
POST /admin/translate
  text      描述（如「哆啦A梦里面的野比玉子」）
  generate  true = 转译后立刻出图；false = 只返回提示词文本
  size      可选 "宽x高"，不填则用转译推荐的尺寸
  style     可选，true 时出图前套用面板「画风预设」
  prompt    可选，直接给总提示词（跳过转译，面板「再出一张」用）
  negative  可选，配合 prompt 直接给负面词
```

`generate=true` 时返回体额外带 `preview`（data URL）、`via`、`ext`、`bytes`、`latency_ms`、`used_size`。
若转译成功但出图失败，仍是 `200` + `image_error`（已拿到的提示词不会丢）。

## 部署（Linux 服务器）

```bash
./v-adapter            # 前台运行
# 或 systemd（WorkingDirectory 决定 config.json / data/ 落点）
```

防火墙放行 8888 端口。若要改端口：面板改 `listen`（重启生效）或改 `config.json`。

## 许可证与免责

以 **MIT License**（作者 VILK，详见 `LICENSE` 文件）授权发布。

- 允许自由使用、复制、修改（**二次创作**）、**二次传播**与商用；
- 二次创作与再分发时**必须保留原作者署名（VILK）**及本许可声明；
- **仅供学习和研究使用**，请遵守上游服务条款，使用者自行承担风险；
- 本软件按「现状」提供，不附带任何明示或暗示的担保，作者不对使用本程序产生的任何后果负责。
