# go-shell

单二进制的流式对话 CLI + HTTP API，对话引擎与 HTTP 服务同进程运行，不 fork 子进程。

三种运行模式：

| 命令 | 模式 |
|---|---|
| `go-shell "问题"` | 本机终端：引擎在本进程，直接输出 |
| `go-shell server` | 远程服务端：监听 HTTP，供别的机器连 |
| `go-shell -h 别名 "问题"` | 远程客户端：连远端 server，内容仍在本终端输出 |

## 构建与运行

```bash
GOPROXY=https://goproxy.cn,direct go mod tidy
GOPROXY=https://goproxy.cn,direct go build -o go-shell .
cp .env.example .env          # 首次：填 API Key 等
```

```bash
./go-shell "你好"              # 默认复用最近活跃会话
./go-shell -n "新话题"         # 终结当前会话并新开
./go-shell -s / -s <id>        # 列会话 / 继续指定会话
./go-shell -S / -S <名称>      # 列 skill / 切换 disable
./go-shell -I / -I <名称> / -D <名称>   # 远程 skill 列表 / 安装 / 删除
./go-shell -k <key> / -m <model>        # 改 API Key / 模型，写回当前配置文件
./go-shell -c /path/to/.env server      # 启动 API
./go-shell -h nuc5 "你好"       # 连 nuc5，内容在本终端输出
```

## 配置位置

查找顺序（决定 `go.config`、`data/`、`skill/` 在哪）：

1. `-c/--config <路径>`
2. `GO_SHELL_HOME` / `XDG_CONFIG_HOME`
3. `~/.config/go-shell/go.config`（存在就用）
4. `<二进制目录>/.env`（老部署，存在就用，配置与数据都留在原地）
5. 都没有 → 在 `~/.config/go-shell/` 生成 `go.config`，并建好 `data/`、`skill/`

Windows 同为 `%USERPROFILE%\.config\go-shell`。配置模板 `//go:embed` 在二进制里，
机器上只有 `go-shell` 一个文件也能生成完整配置。

`-c` 只改「配置文件在哪」；`data/`、`skill/` 仍跟二进制目录走，除非该配置目录里本来就有
`data/` 或 `skill/`（此时它也算配置根）。要完全分开就用 `DATA_DIR` / `SKILL_DIR`。

## 功能

- LLM 双协议：DeepSeek Responses API / OpenAI 兼容 Chat Completions，流式输出
- 工具：get_system_info / execute_system_command / skill / skill_hub（可选 web_search）
- skill：`skill/<名称>/skill.py` 由 `PYTHON_BIN` 子进程执行，runner 经 `//go:embed` 内嵌
  （运行时写临时文件），并自动注入 `core.tools.envelope` 兼容层，部署目录无需带 `core/`
- 存储：本地 SQLite 唯一读写源 + 在线 MySQL 幂等同步（uid 去重 / synced 标记）
- OpenViking：question/step 记忆注入、profile 索引、session 抓取与 commit
- 计费：DeepSeek / OpenCode Go 峰谷单价、费用与余额查询
- 远程客户端：`-h 别名` 连远端，SSE 事件还原成本地终端渲染，与本地同一套事件协议

## 配置（.env）

```bash
SERVER_HOST=127.0.0.1   # 127.0.0.1 仅本机 / 0.0.0.0 所有网卡
SERVER_PORT=8000
API_ACCESS_KEY=         # 全局验证码：≥8 位生效，留空或过短 = 不校验
INTERJECT_GRACE=45      # 插话等待落地的最长秒数
PYTHON_BIN=python3      # skill 解释器；带目录时该目录自动前置到子进程 PATH
SKILL_DIR=              # 默认 <配置根>/skill
DATA_DIR=               # 默认 <配置根>/data，SQLite 落这里

REMOTE_HOSTS=           # 别名=地址:端口:访问码，逗号分隔多个
REMOTE_HOST=            # -h 不带值时的默认目标
REMOTE_PORT=8000
REMOTE_KEY=             # 默认访问码（= 对端 API_ACCESS_KEY）
REMOTE_TIMEOUT=15       # 建连 / 健康检查超时（秒），流式对话不设总超时
```

其余项（API Key、模型、OpenViking、MySQL、工具轮数等）见 `.env.example`。

## 远程客户端（`-h`）

```bash
# 配置里写好别名（访问码 = 对端 API_ACCESS_KEY）
REMOTE_HOSTS=nuc5=192.0.2.10:8018:yourkey1,aml=192.0.2.11:8018

go-shell -h nuc5 "查看磁盘占用"      # -h +nuc5 等价
go-shell -h nuc5 -n "新话题"
go-shell -h nuc5 -s                  # 列远端会话；-s <id> 继续
go-shell -h nuc5 -S                  # 远端 skill 列表；-I / -D 同样发到远端
go-shell -h 192.0.2.10:8018 "…"  # 不配别名，直接写地址（沿用 REMOTE_PORT/KEY）
```

客户端只做「发请求 + 渲染」：不读写本地数据库、不需要 `DEEPSEEK_API_KEY`，会话与数据都在远端。
`-k/-m` 在远程模式下会被忽略（不改远端配置）。连不上或验证码不对时，报错会直接指明是地址、
端口还是 `API_ACCESS_KEY`。

## HTTP API

`API_ACCESS_KEY` 生效时，除 `/health` 外都要带 `Authorization: Bearer <code>`。

| 接口 | 说明 |
|---|---|
| `GET /health` | 健康检查，免鉴权 |
| `POST /chat` | 同步对话，`{"question":"...","new":false}` |
| `POST /chat/stream` | SSE 流式对话 |
| `POST /interject` | 中途插话 |
| `GET /interject/status` | `{access_required, grace, turn_id}`，判断能否插话 |

`/chat/stream` 事件：`start/done/error/content/reasoning/tool_call/tool_result/usage/
question/db/session/memory/tool_round/continuing/warning/search/log/turn/interject`，
`: heartbeat` 为保活注释。`turn` 给出本轮 id，插话时用它指定目标轮次。

## 中途插话

一轮还在跑时改主意，不必杀进程重来：把话递进本轮，等它走到**工具边界**（一批工具结果刚回来、
下次请求未发出）时作为 `user` 消息进上下文，模型下一步就能改道。插话按新任务处理，工具预算重算。

| 返回 | 含义 | 客户端 |
|---|---|---|
| `{"status":"injected","turn_id":"t7"}` | 已进上下文并落库 | 别重发，继续看这条流 |
| `{"status":"timeout"}` | 没等到工具边界 | 按新消息重发 |
| `{"status":"expired"}` | 本轮已结束 | 按新消息重发 |
| `{"status":"idle"}`（409） | 当前没有在跑的轮次 | 直接发新消息 |

落点只能是整批工具结果之后：夹在两批并行 `function_call_output` 中间会留下没有 output 的
孤立 `function_call`，两种协议都 400 且已写进历史，之后每轮重放都会再错。

## 实现要点

- 引擎与 HTTP 服务同进程，事件经 channel 直达 SSE
- 插话用轮次注册表 + 队列，不依赖阻塞式的 stdin 管道；回执只在落库之后发出
- 客户端模式把 SSE 的 `type` 还原成事件层 `t`，直接复用终端渲染，不另写一套输出
- 终端渲染为简化版（无 rich 面板）

## 目录

`main.go` 入口 · `internal/{chat,llm,tools,events,server,client,db,config,billing,interject,recall,envelope}`
