import test from "node:test";
import assert from "node:assert/strict";
import { readCompletionStream } from "../internal/proxy/web/sse.mjs";
const encoder = new TextEncoder();
function body(parts, close = true, cancel = () => {}) {
  return new ReadableStream({
    start(controller) {
      for (const part of parts) controller.enqueue(encoder.encode(part));
      if (close) controller.close();
    },
    cancel,
  });
}
test(
  "DONE completes an open connection and cancels the reader",
  { timeout: 1000 },
  async () => {
    let cancelled = false;
    const events = [];
    await readCompletionStream(
      body(
        ['data: {"value":1}\n\ndata: [DONE]\n\n'],
        false,
        () => (cancelled = true),
      ),
      (e) => events.push(e),
    );
    assert.equal(cancelled, true);
    assert.deepEqual(events, [{ value: 1 }]);
  },
);
test("CRLF and UTF-8 characters can span network chunks", async () => {
  const text = encoder.encode(
    'data: {"text":"Привет 🌍"}\r\n\r\ndata: [DONE]\r\n\r\n',
  );
  const stream = new ReadableStream({
    start(c) {
      for (const byte of text) c.enqueue(Uint8Array.of(byte));
      c.close();
    },
  });
  const events = [];
  await readCompletionStream(stream, (e) => events.push(e));
  assert.deepEqual(events, [{ text: "Привет 🌍" }]);
});
test("truncated and invalid streams fail and release resources", async () => {
  await assert.rejects(
    readCompletionStream(body(["data: {}\n\n"]), () => {}),
    /interrupted/,
  );
  let cancelled = false;
  await assert.rejects(
    readCompletionStream(
      body(["data: not-json\n\n"], false, () => (cancelled = true)),
      () => {},
    ),
    SyntaxError,
  );
  assert.equal(cancelled, true);
  await assert.rejects(
    readCompletionStream(
      body(['data: {"error":{"message":"bad upstream"}}\n\n']),
      () => {},
    ),
    /bad upstream/,
  );
});
test("large unterminated events are bounded", async () => {
  await assert.rejects(
    readCompletionStream(body(["data: " + "x".repeat(1 << 20)]), () => {}),
    /size limit/,
  );
});
