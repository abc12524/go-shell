# runner.py：go-shell 的 skill 执行 shim（经 //go:embed 打进二进制，运行时写入临时文件执行）
#
# 职责：把项目根目录加进 sys.path 后按包路径导入 skill.<名称>.skill，调用 run(arguments)，
# 返回统一信封字符串。技能内的相对导入 / config.json 读取逻辑不受影响。
# 兼容层：目标环境没有 core 包时，向 sys.modules 注入内存版 core.tools.envelope，
# 使既有 skill 的 "from core.tools.envelope import ..." 正常工作（磁盘无需 core/ 目录）。
import importlib
import importlib.util
import json
import sys
import types


def _ensure_envelope_shim():
    """磁盘上没有 core 包时注入内存版 core.tools.envelope（信封协议与 internal/envelope 一致）。"""
    try:
        if importlib.util.find_spec("core") is not None:
            return  # 目标环境有真实 core 包，不动
    except Exception:
        pass

    def ok(result=None):
        if result is None:
            result = {}
        return json.dumps({"status": "ok", "result": result}, ensure_ascii=False)

    def error(message, code=None, **extra):
        payload = {"status": "error", "error": str(message)}
        if code is not None:
            payload["code"] = code
        for key, value in extra.items():
            if value is not None:
                payload[key] = value
        return json.dumps(payload, ensure_ascii=False)

    def is_error(value):
        return isinstance(value, dict) and value.get("status") == "error"

    core = types.ModuleType("core")
    tools_mod = types.ModuleType("core.tools")
    env_mod = types.ModuleType("core.tools.envelope")
    env_mod.ok = ok
    env_mod.error = error
    env_mod.is_error = is_error
    core.tools = tools_mod
    tools_mod.envelope = env_mod
    sys.modules["core"] = core
    sys.modules["core.tools"] = tools_mod
    sys.modules["core.tools.envelope"] = env_mod


def main():
    root, module, args_json = sys.argv[1], sys.argv[2], sys.argv[3]
    if root not in sys.path:
        sys.path.insert(0, root)
    _ensure_envelope_shim()
    mod = importlib.import_module(module)
    run = getattr(mod, "run", None)
    if not callable(run):
        print(json.dumps(
            {"status": "error", "error": f"skill 脚本缺少 run(arguments) 函数", "code": "internal"},
            ensure_ascii=False))
        return
    arguments = json.loads(args_json) if args_json else {}
    result = run(arguments)
    if isinstance(result, str):
        print(result)
    else:
        print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
