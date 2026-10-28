import { readCompletionStream } from "./sse.mjs";

const $ = (id) => document.getElementById(id);
let history = [],
  controller = null;
async function health() {
  try {
    const r = await fetch("/readyz");
    if (!r.ok) throw Error();
    $("health").classList.remove("offline");
    $("health-label").textContent = "Gateway ready";
  } catch {
    $("health").classList.add("offline");
    $("health-label").textContent = "Gateway unavailable";
  }
}
health();
setInterval(health, 30000);
function message(role, text) {
  $("empty")?.remove();
  const el = document.createElement("div");
  el.className = "message " + role;
  const label = document.createElement("div");
  label.className = "role";
  label.textContent = role;
  const body = document.createElement("div");
  body.className = "text";
  body.textContent = text;
  el.append(label, body);
  $("messages").append(el);
  return body;
}
function scroll() {
  $("messages").scrollTop = $("messages").scrollHeight;
}
function metric(id, value, unit = "") {
  const el = $(id);
  el.textContent = value;
  if (unit) {
    const small = document.createElement("small");
    small.textContent = " " + unit;
    el.append(small);
  }
}
$("model").onchange = () => {
  history = [];
  $("messages").replaceChildren();
  $("status-text").textContent = "Model changed · conversation cleared.";
};
$("stop").onclick = () => controller?.abort();
$("clear").onclick = () => {
  if (controller) return;
  history = [];
  $("messages").replaceChildren();
  $("status-text").textContent = "Conversation cleared.";
};
document.querySelectorAll("[data-prompt]").forEach(
  (b) =>
    (b.onclick = () => {
      $("prompt").value = b.dataset.prompt;
      $("prompt").focus();
    }),
);
$("prompt").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    if (!controller) $("form").requestSubmit();
  }
});
$("form").onsubmit = async (e) => {
  e.preventDefault();
  if (controller) return;
  const prompt = $("prompt").value.trim();
  if (!prompt) return;
  const max = Number($("tokens").value);
  if (!Number.isInteger(max) || max < 1 || max > 32768) {
    $("tokens").reportValidity();
    return;
  }
  controller = new AbortController();
  $("send").disabled = true;
  for (const id of ["model", "key", "tokens", "stream", "prompt"])
    $(id).disabled = true;
  $("clear").disabled = true;
  $("stop").hidden = false;
  $("status-text").textContent = "Request in flight…";
  for (const id of ["latency", "ttft", "usage", "http-status"]) metric(id, "—");
  message("user", prompt);
  const output = message("assistant", "");
  scroll();
  let reply = "",
    first = null,
    usage = null,
    success = false;
  const started = performance.now();
  const streaming = $("stream").checked;
  const pending = [...history, { role: "user", content: prompt }];
  try {
    const headers = { "Content-Type": "application/json" };
    if ($("key").value) headers.Authorization = "Bearer " + $("key").value;
    const body = {
      model: $("model").value,
      messages: pending,
      stream: streaming,
      max_tokens: max,
    };
    if (streaming) body.stream_options = { include_usage: true };
    const r = await fetch("/v1/chat/completions", {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      signal: controller.signal,
    });
    metric("http-status", r.status);
    if (!r.ok) {
      let reason = "Request failed";
      try {
        reason = (await r.json()).error.message;
      } catch {}
      throw Error(reason);
    }
    if (streaming) {
      await readCompletionStream(r.body, (event) => {
        const text = event.choices?.[0]?.delta?.content || "";
        if (text && first === null) first = performance.now() - started;
        reply += text;
        output.textContent = reply;
        if (event.usage) usage = event.usage;
        scroll();
      });
    } else {
      const result = await r.json();
      reply = result.choices[0].message.content;
      usage = result.usage;
      output.textContent = reply;
    }
    metric("usage", usage?.total_tokens ?? "Unavailable");
    history = [...pending, { role: "assistant", content: reply }];
    $("prompt").value = "";
    success = true;
    $("status-text").textContent =
      "Completed · " + (r.headers.get("X-Request-ID") || "");
  } catch (err) {
    const cancelled = err.name === "AbortError";
    if (!reply)
      output.textContent = cancelled ? "Request stopped." : err.message;
    else
      message(
        "error",
        cancelled
          ? "Request stopped. Partial response was not added to conversation."
          : err.message,
      );
    $("status-text").textContent = cancelled
      ? "Stopped by you."
      : "Request failed · " + err.message;
  } finally {
    metric("latency", Math.round(performance.now() - started), "ms");
    if (first !== null) metric("ttft", Math.round(first), "ms");
    controller = null;
    $("send").disabled = false;
    for (const id of ["model", "key", "tokens", "stream", "prompt"])
      $(id).disabled = false;
    $("clear").disabled = false;
    $("stop").hidden = true;
    scroll();
    if (success) $("prompt").focus();
  }
};
