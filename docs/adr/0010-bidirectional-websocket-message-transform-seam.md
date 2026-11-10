# Add an opt-in bidirectional WebSocket message-transform seam

**Status:** merged into
[ADR-0006](0006-openai-responses-websocket-transport.md)

This record accepted the opt-in `ClientMessageTransformer` /
`ServerMessageTransformer` seam on the OpenAI Responses WebSocket transport,
reversing ADR-0006's original hook-free stance. Its decision, considered
options, and consequences now live in ADR-0006, so the transport has one
decision record. The number is retained so existing references resolve.
