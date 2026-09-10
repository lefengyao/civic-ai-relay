"""本地假上游：模拟 OpenAI 兼容的 /v1/models 与 /v1/chat/completions，用于拉取模型功能自测。"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

MODELS = [
    {"id": "gpt-4o-mini", "object": "model", "owned_by": "fake"},
    {"id": "claude-haiku-fake", "object": "model", "owned_by": "fake"},
    {"id": "deepseek-chat", "object": "model", "owned_by": "fake"},
    {"id": "qwen-turbo", "object": "model", "owned_by": "fake"},
    {"id": "embedding-fake", "object": "model", "owned_by": "fake"},
]


class Handler(BaseHTTPRequestHandler):
    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._json({"object": "list", "data": MODELS})
        else:
            self._json({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        if self.path.startswith("/v1/chat/completions"):
            self._json({"id": "chatcmpl-fake", "object": "chat.completion",
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": "pong"},
                                     "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 1000000, "completion_tokens": 1000000, "total_tokens": 2000000}})
        else:
            self._json({"error": "not found"}, 404)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8899), Handler).serve_forever()
