# A two-minute walkthrough

1. Start `go run ./cmd/gateway`, then open http://localhost:8080. Explain that the mock makes the service demonstrable without external credentials.
2. Send a prompt with streaming enabled. Show the incremental reply, request ID, round-trip time and usage. The mock echoes text; it is not a model.
3. Turn streaming off and send another prompt. The same endpoint returns a normal JSON completion.
4. Run the example curl request from the README to show that the UI is a client of the public API.
5. Send a request with a model outside `GATEWAY_MODELS` and show the structured 400 error.
6. Start the Compose stack and send several requests. Open Grafana's LLM Gateway dashboard to inspect latency, token throughput and stream outcomes.
7. Explain tenant keys, process-local request limits, cancellation and the decision to make only one upstream attempt. Describe distributed budgets as an explicit deployment boundary rather than an implemented feature.

For a live model demonstration, configure a compatible upstream and tenant key before presenting. Check its availability and provider spending limits in advance. Never show environment contents or paste a real upstream key into the playground.
