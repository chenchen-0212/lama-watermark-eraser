#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
big-lama 去水印引擎常驻子进程。

协议：stdin/stdout 上的 JSONL（每行一个 JSON），统一信封 {type,id,ok,data,error}。
  - Go -> 本进程: hello / infer / ping / cancel / shutdown
  - 本进程 -> Go: 对应响应；错误时 ok:false + error.code
  - stdout 只走协议（UTF-8/ASCII 安全）；人类可读日志一律走 stderr。

图像约定：RGB888 行优先 base64；掩膜：单通道 0/255 base64。
启动流程：收到 hello -> 加载模型（torch.jit.load）-> 回 ready；单张 infer 异常不退出。

用法：
  python worker.py --model D:/path/to/big-lama.pt
  （--model 缺省时自动在 exe 同级 / _internal / models / cwd 下定位 big-lama.pt）
"""
import argparse
import base64
import json
import os
import sys
import traceback

import numpy as np

# Windows 控制台 GBK 防护：stdout/stdin 强制 UTF-8（尽力而为，失败不致命）
for _stream in (sys.stdout, sys.stdin):
    try:
        _stream.reconfigure(encoding="utf-8")
    except Exception:
        pass


def _log(msg):
    """日志一律走 stderr，避免污染 stdout 协议流。"""
    print(msg, file=sys.stderr, flush=True)


def _send(msg):
    """写一行 JSON 到 stdout 并立即 flush（Go 侧按行读取）。"""
    line = json.dumps(msg, ensure_ascii=True)
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def _err(typ, rid, code, message):
    return {
        "type": typ,
        "id": rid,
        "ok": False,
        "error": {"code": code, "message": message},
    }


def _resolve_model_path(explicit):
    """定位 big-lama.pt：显式参数优先，其次 exe 同级 / _internal / models / cwd。"""
    candidates = []
    if explicit:
        candidates.append(explicit)

    meipass = getattr(sys, "_MEIPASS", None)
    exe_dir = os.path.dirname(os.path.abspath(sys.executable))
    here = os.path.dirname(os.path.abspath(__file__))

    bases = []
    if meipass:
        bases.append(meipass)
    bases += [
        exe_dir,
        os.path.join(exe_dir, "_internal"),
        here,
        os.path.join(here, "models"),
        os.getcwd(),
    ]
    rels = ["models/big-lama.pt", "big-lama.pt"]
    for base in bases:
        for rel in rels:
            p = os.path.join(base, rel)
            if os.path.isfile(p):
                candidates.append(p)

    for p in candidates:
        if os.path.isfile(p):
            return p
    raise FileNotFoundError(
        "找不到 big-lama.pt 权重文件（已尝试: %s）"
        % (" | ".join(candidates) if candidates else "<无>")
    )


class Worker:
    """常驻推理服务：加载一次模型，逐请求复用。"""

    def __init__(self, explicit_model, device="cpu", num_threads=0):
        self.explicit_model = explicit_model
        self.device = device
        self.num_threads = num_threads
        self.core = None   # inpaint_core 模块（延迟导入，控制 torch 加载时机）
        self.model = None

    def handle_hello(self, rid, data):
        if self.model is None:
            model_path = _resolve_model_path(self.explicit_model)
            _log("loading model from: %s" % model_path)

            import inpaint_core as core
            self.core = core

            # num_threads：hello.data 优先，其次 CLI；<=0 表示系统默认（不显式设置）
            nt = self.num_threads
            if isinstance(data, dict) and data.get("num_threads"):
                try:
                    nt = int(data["num_threads"])
                except (TypeError, ValueError):
                    nt = self.num_threads
            if nt and nt > 0:
                core.set_num_threads(nt)

            self.model = core.load_model(model_path, device=self.device)
            _log("model ready")
        return {"type": "hello", "id": rid, "ok": True, "data": {"status": "ready"}}

    def handle_infer(self, rid, data):
        if self.model is None or self.core is None:
            raise RuntimeError("模型未加载，请先发送 hello")
        w = int(data.get("width", 0))
        h = int(data.get("height", 0))
        if w <= 0 or h <= 0:
            raise ValueError("无效图像尺寸: %dx%d" % (w, h))

        img_bytes = base64.b64decode(data.get("image_b64", ""))
        mask_bytes = base64.b64decode(data.get("mask_b64", ""))
        if len(img_bytes) != w * h * 3:
            raise ValueError(
                "image_b64 字节数不符: 期望 %d 实得 %d" % (w * h * 3, len(img_bytes)))
        if len(mask_bytes) != w * h:
            raise ValueError(
                "mask_b64 字节数不符: 期望 %d 实得 %d" % (w * h, len(mask_bytes)))

        image = np.frombuffer(img_bytes, dtype=np.uint8).reshape(h, w, 3)
        mask = np.frombuffer(mask_bytes, dtype=np.uint8).reshape(h, w)

        out = self.core.forward(self.model, image, mask)  # [H, W, 3] uint8 RGB
        out_bytes = out.tobytes()
        return {
            "type": "infer",
            "id": rid,
            "ok": True,
            "data": {
                "width": w,
                "height": h,
                "image_b64": base64.b64encode(out_bytes).decode("ascii"),
            },
        }

    def dispatch(self, req):
        rid = req.get("id", 0)
        mtype = req.get("type")
        data = req.get("data") or {}
        try:
            if mtype == "hello":
                return self.handle_hello(rid, data)
            if mtype == "infer":
                return self.handle_infer(rid, data)
            if mtype == "ping":
                return {"type": "ping", "id": rid, "ok": True,
                        "data": {"status": "pong"}}
            if mtype == "cancel":
                # 尽力而为；真正可靠的取消由 Go 侧 kill 进程树实现
                return {"type": "cancel", "id": rid, "ok": True,
                        "data": {"status": "canceled"}}
            if mtype == "shutdown":
                return {"type": "shutdown", "id": rid, "ok": True,
                        "data": {"status": "bye"}}
            return _err(mtype, rid, "INVALID_REQUEST", "未知消息类型: %s" % mtype)
        except Exception as exc:  # 单张异常不退出进程
            _log("handle %s failed: %s\n%s" % (mtype, exc, traceback.format_exc()))
            if mtype == "infer":
                code = "INVALID_IMAGE" if isinstance(exc, ValueError) else "INFER_FAILED"
            elif mtype == "hello":
                code = "MODEL_LOAD_FAILED"
            else:
                code = "INVALID_REQUEST"
            return _err(mtype, rid, code, str(exc))

    def run(self):
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except Exception as exc:
                _send(_err("event", 0, "INVALID_REQUEST", "非法 JSON: %s" % exc))
                continue
            resp = self.dispatch(req)
            _send(resp)
            if req.get("type") == "shutdown":
                return


def main():
    ap = argparse.ArgumentParser(description="big-lama 去水印引擎常驻子进程")
    ap.add_argument("--model", default=None, help="big-lama.pt 路径（缺省自动定位）")
    ap.add_argument("--device", default="cpu", help="推理设备（当前仅 cpu）")
    ap.add_argument("--num-threads", type=int, default=0,
                    help="torch 线程数（0=系统默认）")
    args = ap.parse_args()

    worker = Worker(args.model, device=args.device, num_threads=args.num_threads)
    _log("worker started; device=%s" % args.device)
    worker.run()


if __name__ == "__main__":
    main()
