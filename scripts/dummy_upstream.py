#!/usr/bin/env python3
"""Dummy Anthropic Messages upstream for scripts/e2e_cpa_quota.sh.

It answers POST /v1/messages, streaming or not, for any model, with fixed
token usage, and reports Claude's unified quota headers: every answered
request raises the 5-hour utilization by one point and the 7-day one by a
sixth of a point. A request whose text contains "E2E REFUSAL" is refused with
stop_reason "refusal" and a cyber stop_details block, which CLIProxyAPI turns
into a 400 policy refusal.

GET /ratio_config serves a new-api ratio table pricing the test model, so the
suite does not depend on the public price server.
"""

import argparse
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

INPUT_TOKENS = 1000
OUTPUT_TOKENS = 200
CACHE_READ_TOKENS = 3000
CACHE_WRITE_TOKENS = 400
REFUSAL_MARKER = "E2E REFUSAL"
REFUSAL_EXPLANATION = "This request was refused by the dummy upstream."

RATIO_TABLE = {
    "success": True,
    "data": {
        "model_ratio": {"claude-e2e-model": 2.5},
        "completion_ratio": {"claude-e2e-model": 5},
        "cache_ratio": {"claude-e2e-model": 0.1},
        "create_cache_ratio": {"claude-e2e-model": 1.25},
    },
}


class Quota:
    """The account's hidden utilization, shared by all requests."""

    def __init__(self):
        self.lock = threading.Lock()
        self.requests = 0
        self.start = int(time.time())

    def next_headers(self):
        with self.lock:
            used5 = min(self.requests * 0.01, 1.0)
            used7 = min(self.requests * 0.01 / 6, 1.0)
            self.requests += 1
        return {
            "anthropic-ratelimit-unified-status": "allowed",
            "anthropic-ratelimit-unified-5h-utilization": f"{used5:.6f}",
            "anthropic-ratelimit-unified-5h-reset": str(self.start + 5 * 3600),
            "anthropic-ratelimit-unified-7d-utilization": f"{used7:.6f}",
            "anthropic-ratelimit-unified-7d-reset": str(self.start + 7 * 24 * 3600),
        }


QUOTA = Quota()


def usage():
    return {
        "input_tokens": INPUT_TOKENS,
        "output_tokens": OUTPUT_TOKENS,
        "cache_read_input_tokens": CACHE_READ_TOKENS,
        "cache_creation_input_tokens": CACHE_WRITE_TOKENS,
    }


def stop_fields(refused):
    if not refused:
        return {"stop_reason": "end_turn"}
    return {
        "stop_reason": "refusal",
        "stop_details": {"type": "refusal", "category": "cyber", "explanation": REFUSAL_EXPLANATION},
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def send_json(self, status, body, headers=None):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path.split("?")[0] == "/ratio_config":
            self.send_json(200, RATIO_TABLE)
            return
        self.send_json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        try:
            request = json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            self.send_json(400, {"type": "error", "error": {"type": "invalid_request_error", "message": "bad json"}})
            return
        path = self.path.split("?")[0]
        if path.endswith("/count_tokens"):
            self.send_json(200, {"input_tokens": INPUT_TOKENS})
            return
        if not path.endswith("/v1/messages"):
            self.send_json(404, {"type": "error", "error": {"type": "not_found_error", "message": path}})
            return
        model = request.get("model", "claude-e2e-model")
        refused = REFUSAL_MARKER in json.dumps(request.get("messages", []))
        headers = QUOTA.next_headers()
        message_id = "msg_" + uuid.uuid4().hex
        if not request.get("stream"):
            body = {
                "id": message_id,
                "type": "message",
                "role": "assistant",
                "model": model,
                "content": [{"type": "text", "text": "" if refused else "OK"}],
                "stop_sequence": None,
                "usage": usage(),
            }
            body.update(stop_fields(refused))
            self.send_json(200, body, headers)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        for key, value in headers.items():
            self.send_header(key, value)
        self.end_headers()
        start_usage = usage()
        start_usage["output_tokens"] = 1
        events = [
            ("message_start", {"type": "message_start", "message": {"id": message_id, "type": "message", "role": "assistant",
                                                                    "model": model, "content": [], "stop_reason": None,
                                                                    "stop_sequence": None, "usage": start_usage}}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": "" if refused else "OK"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": dict(stop_fields(refused), stop_sequence=None),
                               "usage": {"output_tokens": OUTPUT_TOKENS}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        for name, data in events:
            self.wfile.write(f"event: {name}\ndata: {json.dumps(data)}\n\n".encode())
            self.wfile.flush()
        self.close_connection = True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0)
    args = parser.parse_args()
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(server.server_address[1], flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
