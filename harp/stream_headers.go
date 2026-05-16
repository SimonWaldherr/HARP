package harp

const (
	// StreamHeader marks a response as part of a streamed/chunked sequence.
	StreamHeader = "X-Harp-Stream"
	// StreamEndHeader marks the final chunk of a streamed/chunked response.
	StreamEndHeader = "X-Harp-Stream-End"
)
