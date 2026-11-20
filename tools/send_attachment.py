"""Send files explicitly selected by the user to a Responses endpoint."""
import argparse
import base64
import json
import os
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

MIB = 1024 * 1024


def request_body(model, prompt, paths):
    if not 1 <= len(paths) <= 16:
        raise ValueError("一次请选择 1–16 个文件。")
    content = [{"type": "input_text", "text": prompt}]
    remaining = 60 * MIB
    for path in paths:
        if not path.is_file():
            raise ValueError(f"无法读取文件：{path.name}")
        limit = min(20 * MIB, remaining)
        with path.open("rb") as f:
            data = f.read(limit + 1)
        if not data or len(data) > limit:
            raise ValueError(f"文件为空或超过上传大小限制：{path.name}")
        remaining -= len(data)
        content.append({"type": "input_file", "filename": path.name,
                        "file_data": base64.b64encode(data).decode("ascii")})
    return json.dumps({"model": model, "stream": False,
                       "input": [{"role": "user", "content": content}]}).encode()


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def send(url, key, data):
    target = urlsplit(url)
    local = target.hostname in {"localhost", "127.0.0.1", "::1"}
    if (not target.hostname or target.username is not None or target.query or target.fragment
        or (target.scheme != "https" and not (local and target.scheme == "http"))
        or not target.path.endswith("/v1/responses")):
        raise ValueError("请指定 HTTPS 的 /v1/responses 地址（本机测试可使用 HTTP）。")
    request = Request(url, data=data, headers={"Content-Type": "application/json",
                                                "Authorization": "Bearer " + key})
    try:
        response = build_opener(NoRedirect()).open(request, timeout=180)
    except HTTPError as error:
        response = error
    with response:
        try:
            result = json.loads(response.read(8 * MIB))
            if not isinstance(result, dict):
                raise ValueError("expected a response object")
        except (UnicodeError, ValueError):
            raise ValueError(f"请求失败（HTTP {response.status}，响应不是有效 JSON）。") from None
        if not 200 <= response.status < 300:
            error = result.get("error")
            message = error.get("message") if isinstance(error, dict) else None
            raise ValueError(message or f"请求失败（HTTP {response.status}）。")
        return result.get("output_text") or "".join(
            c.get("text", "") for item in result.get("output", [])
            if item.get("type") == "message" for c in item.get("content", [])
            if c.get("type") == "output_text"
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="完整的 /v1/responses 地址")
    parser.add_argument("--model", required=True)
    parser.add_argument("--prompt", required=True)
    parser.add_argument("files", nargs="+", type=Path)
    args = parser.parse_args()
    key = os.environ.get("SUB2API_API_KEY")
    if not key:
        parser.error("请先设置 SUB2API_API_KEY 环境变量。")
    try:
        print(send(args.url, key, request_body(args.model, args.prompt, args.files)))
        return 0
    except (OSError, URLError, ValueError) as error:
        print(error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
