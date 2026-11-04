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
- HTTP API：`/chat`（同步）、`/chat/stream`（SSE）、`/health`；事件协议与 Python 版一致
- 计费：DeepSeek/OpenCode Go 峰谷单价、费用与余额查询

## 服务端配置（.env）

```bash
SERVER_HOST=127.0.0.1   # 绑定 IP：127.0.0.1（仅本机）或 0.0.0.0（所有网卡）
SERVER_PORT=8000        # 监听端口
PYTHON_BIN=/root/.venv/bin/python3   # skill 脚本解释器
PYTHON_PATH=                         # 可选：冒号分隔目录，前置到 skill 子进程 PATH
```

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

```bash
curl http://127.0.0.1:8000/health
curl -X POST http://127.0.0.1:8000/chat \
  -H 'Content-Type: application/json' \
  -d '{"question":"查看磁盘占用","new":false}'
curl -N -X POST http://127.0.0.1:8000/chat/stream \
  -H 'Content-Type: application/json' \
  -d '{"question":"你好"}'
```

`/chat/stream` SSE 事件：`start/done/error/content/reasoning/tool_call/tool_result/
usage/question/db/session/memory/tool_round/continuing/warning/search/log`，
字段与 Python 版一致；`: heartbeat` 为保活注释。

## 与 Python 版的差异

- server 不再 fork `dp.py` 子进程：引擎在本进程内执行，事件经 channel 直达 SSE
- skill 执行为 `PYTHON_BIN` 子进程：runner 经 `//go:embed` 内嵌进二进制（运行时写临时文件），
  `core.tools.envelope` 兼容层由 runner 在内存注入（部署目录无需 runner.py 与 core/）
- 终端渲染为简化版（无 rich 面板）；`client/dp_client.py` 客户端未复刻（用 curl 或后续补）
