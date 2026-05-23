package harp

const (
	// StreamHeader marks a response as part of a streamed/chunked sequence.
	StreamHeader = "X-Harp-Stream"
	// StreamEndHeader marks the final chunk of a streamed/chunked response.
	StreamEndHeader = "X-Harp-Stream-End"
	// StreamTypeHeader identifies the kind of streamed response.
	StreamTypeHeader = "X-Harp-Stream-Type"
)

const (
	// StreamTypeChunked is the default raw chunked transfer mode.
	StreamTypeChunked = "chunked"
	// StreamTypeSSE applies Server-Sent Events response defaults.
	StreamTypeSSE = "sse"
	// StreamTypeNDJSON applies newline-delimited JSON response defaults.
	StreamTypeNDJSON = "ndjson"
	// StreamTypeText applies plain text streaming response defaults.
	StreamTypeText = "text"
)
