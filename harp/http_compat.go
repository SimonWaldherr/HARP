package harp

import "net/http"

// HeaderValuesFromHTTP preserves all values from an http.Header for the
// protocol's binary-safe, repeated-header representation.
func HeaderValuesFromHTTP(h http.Header) []*HTTPHeader {
	out := make([]*HTTPHeader, 0, len(h))
	for name, values := range h {
		copied := append([]string(nil), values...)
		out = append(out, &HTTPHeader{Name: name, Values: copied})
	}
	return out
}

// HeaderMapFromHTTP returns the legacy single-value header map used by older
// HARP clients. New code should also populate HeaderValuesFromHTTP.
func HeaderMapFromHTTP(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if len(values) > 0 {
			out[name] = values[0]
		}
	}
	return out
}

// HTTPHeaderFromProto reconstructs a full http.Header from the new repeated
// representation, falling back to the legacy map when needed.
func HTTPHeaderFromProto(legacy map[string]string, values []*HTTPHeader) http.Header {
	out := make(http.Header)
	if len(values) > 0 {
		for _, header := range values {
			if header == nil || header.Name == "" {
				continue
			}
			for _, value := range header.Values {
				out.Add(header.Name, value)
			}
			if len(header.Values) == 0 {
				out[header.Name] = nil
			}
		}
		return out
	}
	for name, value := range legacy {
		out.Set(name, value)
	}
	return out
}

// BodyBytesFromProto returns the binary-safe body field, falling back to the
// legacy string body for older senders.
func BodyBytesFromProto(legacy string, bodyBytes []byte) []byte {
	if bodyBytes != nil {
		return bodyBytes
	}
	return []byte(legacy)
}

// BodyStringForLegacy mirrors bytes into the old string field for compatibility
// with older HARP peers. It is lossy for arbitrary binary data, so consumers
// should prefer BodyBytesFromProto.
func BodyStringForLegacy(body []byte) string {
	return string(body)
}
