// Consume a completion stream until its sentinel, even if the connection stays open.
export async function readCompletionStream(body, onEvent) {
  if (!body) throw new Error("Response has no stream body");
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) throw new Error("Stream interrupted before completion");
      buffer += decoder.decode(value, { stream: true });
      let boundary;
      while ((boundary = /\r\n\r\n|\n\n|\r\r/.exec(buffer))) {
        const frame = buffer.slice(0, boundary.index);
        buffer = buffer.slice(boundary.index + boundary[0].length);
        if (frame.length > 1 << 20)
          throw new Error("Stream event exceeds size limit");
        const data = frame
          .split(/\r\n|\n|\r/)
          .filter((line) => line.startsWith("data:"))
          .map((line) => line.slice(5).replace(/^ /, ""))
          .join("\n");
        if (!data) continue;
        if (data === "[DONE]") return;
        const event = JSON.parse(data);
        if (event.error)
          throw new Error(event.error.message || "Stream failed");
        onEvent(event);
      }
      if (buffer.length > 1 << 20)
        throw new Error("Stream event exceeds size limit");
    }
  } finally {
    // Stop the upstream immediately after DONE or a parsing failure.
    try {
      await reader.cancel();
    } catch {
      /* Preserve the original failure. */
    }
    reader.releaseLock();
  }
}
