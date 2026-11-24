"""Exercise the shipped container with only Python's standard library."""
import json
import sys
import time
import urllib.error
import urllib.request

base = sys.argv[1].rstrip("/") if len(sys.argv) > 1 else "http://127.0.0.1:8080"


def get(path):
    return urllib.request.urlopen(base + path, timeout=5)


for attempt in range(30):
    try:
        with get("/readyz") as response:
            assert json.load(response)["status"] == "ok"
        break
    except (OSError, AssertionError):
        if attempt == 29:
            raise
        time.sleep(0.5)

with get("/healthz") as response:
    assert json.load(response)["version"]
with get("/v1/models") as response:
    models = json.load(response)["data"]
    assert models
    assert response.headers["X-Request-ID"]

for stream in (False, True):
    body = {"model": models[0]["id"], "messages": [{"role": "user", "content": "container check"}], "stream": stream}
    if stream:
        body["stream_options"] = {"include_usage": True}
    req = urllib.request.Request(base + "/v1/chat/completions", data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=5) as response:
        assert response.headers["X-Request-ID"]
        if stream:
            assert response.headers.get_content_type() == "text/event-stream"
            events = [line[6:] for line in response.read().decode().splitlines() if line.startswith("data: ")]
            assert events[-1] == "[DONE]"
            assert any(json.loads(event).get("usage") for event in events[:-1])
        else:
            result = json.load(response)
            assert result["choices"][0]["message"]["content"] == "mock response to: container check"
            assert result["usage"]["total_tokens"] > 0

with get("/assets/sse.mjs") as response:
    assert response.headers.get_content_type() == "text/javascript"
    assert "readCompletionStream" in response.read().decode()
with get("/") as response:
    assert "script-src 'self'" in response.headers["Content-Security-Policy"]
with get("/metrics") as response:
    assert "gateway_tokens_total" in response.read().decode()
try:
    get("/v1/usage")
except urllib.error.HTTPError as error:
    assert error.code == 401  # No anonymous access to tenant accounting.
else:
    raise AssertionError("usage endpoint accepted an anonymous request")
print("Container smoke checks passed: probes, models, JSON, SSE, assets, metrics, usage authentication.")
