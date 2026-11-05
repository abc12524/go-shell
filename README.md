# go-shell — shell-tool 的 Go 二进制版

shell-tool（Python）的单二进制重写：DeepSeek 流式 CLI 助手 + HTTP API。
CLI 选项与 Python 版 `dp.py` 一一对应；`go-shell server` 启动 API 服务，
**单进程直跑对话引擎**（不再 fork 子进程）。

## 功能

- 对话 CLI：`go-shell [选项] [问题]`（-n/-s/-S/-I/-D/-k/-m 全部复刻）
- 配置文件：`-c/--config <路径>` 指定（默认 `<二进制目录>/.env`，不存在时自动从
  `.env.example` 生成）；`server` 子命令同样可用 `-c`；`-k/-m` 写回 `-c` 指定的文件
- LLM 双协议：DeepSeek Responses API / OpenAI 兼容 Chat Completions，流式输出
- 工具：get_system_info / execute_system_command / skill / skill_hub（+可选 web_search）
- skill 兼容：`skill/<名称>/skill.py` 仍为 Python 脚本，经内嵌 runner（`//go:embed`）子进程执行
  （`PYTHON_BIN` 指定解释器，复用现有 venv 与既有技能仓库；runner 会在内存注入
  `core.tools.envelope` 兼容层，部署目录无需携带 `core/`）
- 存储：本地 SQLite 唯一读写源 + 在线 MySQL 幂等同步（uid 去重 / synced 标记 / 基线）
- OpenViking：question/step 记忆注入、profile 索引、session 抓取与 commit
- HTTP API：`/chat`（同步）、`/chat/stream`（SSE）、`/interject`（中途插话）、`/health`；
  事件协议与 Python 版一致
- 访问验证码：`.env` 的 `API_ACCESS_KEY`（8 位）。配了则除 `/health` 外都要带对，否则 401
- 中途插话：一轮还在干活时追说的话，等它走到工具边界再进上下文（不杀进程、不丢中途工作）
- 计费：DeepSeek/OpenCode Go 峰谷单价、费用与余额查询

## 服务端配置（.env）

```bash
SERVER_HOST=127.0.0.1   # 绑定 IP：127.0.0.1（仅本机）或 0.0.0.0（所有网卡）
SERVER_PORT=8000        # 监听端口
API_ACCESS_KEY=         # 访问验证码：8 位；空 = 不校验（0.0.0.0 时务必设置）
INTERJECT_GRACE=20      # 中途插话等待落地的最长秒数
PYTHON_BIN=/root/.venv/bin/python3   # skill 脚本解释器
PYTHON_PATH=                         # 可选：冒号分隔目录，前置到 skill 子进程 PATH
```

## 访问验证码

`API_ACCESS_KEY` 是**整个 API 服务**的验证码，不只是插话用的：有值且正好 8 位时，
除 `/health` 外所有接口都要带对，否则回 `401 access_code_required / access_code_invalid`。
三种带法等价，客户端任选：

```bash
-H 'X-Access-Key: 1a2b3c4d'
-H 'Authorization: Bearer 1a2b3c4d'
'http://127.0.0.1:8000/chat?key=1a2b3c4d'
```

留空或长度不是 8 位 = 不校验（本机/内网自用）。`SERVER_HOST=0.0.0.0` 暴露到局域网时务必设置。

## 构建（nuc5，Debian 13 + Go 1.24）

```bash
cd /opt/go-shell
GOPROXY=https://goproxy.cn,direct go mod tidy
GOPROXY=https://goproxy.cn,direct go build -o go-shell .
cp .env.example .env   # 首次：填 API Key 等
```

## 运行

```bash
./go-shell "你好"           # 默认复用最近活跃会话
./go-shell -n "新话题"      # 终结当前会话并新开
./go-shell -s               # 列最近会话；-s <id> 继续
./go-shell -S               # 列 skill；-S <名称> disable 开关
./go-shell -I               # 远程 skill 列表；-I <名称> 安装；-D <名称> 删除
./go-shell server           # 启动 API（端口/绑定 IP 读 .env）
```

## HTTP API

设置了 `API_ACCESS_KEY` 时，除 `/health` 外都要带 `X-Access-Key`（下略）。

```bash
curl http://127.0.0.1:8000/health
curl -X POST http://127.0.0.1:8000/chat \
  -H 'Content-Type: application/json' -H 'X-Access-Key: 1a2b3c4d' \
  -d '{"question":"查看磁盘占用","new":false}'
curl -N -X POST http://127.0.0.1:8000/chat/stream \
  -H 'Content-Type: application/json' -H 'X-Access-Key: 1a2b3c4d' \
  -d '{"question":"你好"}'
```

`/chat/stream` SSE 事件：`start/done/error/content/reasoning/tool_call/tool_result/
usage/question/db/session/memory/tool_round/continuing/warning/search/log/turn/interject`，
字段与 Python 版一致；`: heartbeat` 为保活注释。`turn` 给出本轮 id（插话可用它指定目标），
`interject` 表示某句话已经插进本轮（`{id, text, turn_id}`）。

## 中途插话

一轮还在跑的时候用户改主意，不必杀进程重来：把那句话递给本轮，等它走到**工具边界**
（一批工具结果刚回来、下一次请求还没发）时作为 `user` 消息进入上下文，模型下一步就能
看见并改道。插话按「新任务」处理，工具预算会重新计；这句话以 `<user_interjection>`
包一层并照常落库（会话历史里就是用户说的）。

```bash
curl -X POST http://127.0.0.1:8000/interject \
  -H 'Content-Type: application/json' -H 'X-Access-Key: 1a2b3c4d' \
  -d '{"text":"先别删，改成只读检查"}'
```

| 返回 | 含义 | 客户端该怎么办 |
|---|---|---|
| `{"status":"injected","turn_id":"t7"}` | 已写进上下文并落库 | 不要重发，继续看这条流 |
| `{"status":"timeout"}` | 还没走到工具边界（多卡在长命令上） | 按新消息重发 |
| `{"status":"expired"}` | 这一轮已结束，没赶上 | 按新消息重发 |
| `{"status":"idle"}`（409） | 当前没有正在跑的轮次 | 直接发新消息 |

`GET /interject/status` 返回 `{access_required, grace, turn_id}`，客户端可先问一句再决定
走插话还是重发；`turn_id` 为空即当前没有在跑的轮次。

落点为什么只能在**整批**工具结果之后：夹在两批并行 `function_call_output` 中间会留下
没有 output 的孤立 `function_call`，Responses 与 Chat Completions 都 400，而且它已写进
历史，之后每轮重放都会再错一次。同一原因，插话落不了地时这里不硬杀本轮——没有工具调用
的那一轮根本没有边界可落，`/interject` 会如实回 `timeout`，让客户端重发。

## 与 Python 版的差异

- server 不再 fork `dp.py` 子进程：引擎在本进程内执行，事件经 channel 直达 SSE
- skill 执行为 `PYTHON_BIN` 子进程：runner 经 `//go:embed` 内嵌进二进制（运行时写临时文件），
  `core.tools.envelope` 兼容层由 runner 在内存注入（部署目录无需 runner.py 与 core/）
- 终端渲染为简化版（无 rich 面板）；`client/dp_client.py` 客户端未复刻（用 curl 或后续补）
- 插话不走「父进程写子进程 stdin」那条管道（Python 版需要，因为 turn 在子进程里）：
  go-shell 引擎就在本进程，`internal/interject` 用轮次注册表 + 队列实现同样语义，
  回执（`injected`）仍然只在**落库之后**才发出
