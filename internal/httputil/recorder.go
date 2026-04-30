// Package httputil — see errors.go for the package doc.
package httputil

import (
	"bytes"
	"net/http"
	"sync"
)

// bufPool recycles *bytes.Buffer values to reduce GC pressure when many
// concurrent requests are being recorded.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// ResponseRecorder wraps http.ResponseWriter and captures the HTTP status code
// and full response body written by a handler.
//
// # Streaming note
//
// ResponseRecorder intentionally does NOT implement http.Flusher. Streaming
// (chunked / server-sent events) is deferred to v2. Any handler that requires
// flushing must bypass this recorder.
//
// # Usage
//
//	rec := NewResponseRecorder(w)
//	defer rec.Free()
//	handler.ServeHTTP(rec, r)
//	// rec.Status() and rec.Body() are now available
type ResponseRecorder struct {
	wrapped       http.ResponseWriter
	status        int
	buf           *bytes.Buffer
	headerWritten bool
}

// NewResponseRecorder acquires a *bytes.Buffer from the pool and returns a
// ResponseRecorder that tees all writes to both the wrapped ResponseWriter and
// the internal buffer.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return &ResponseRecorder{
		wrapped: w,
		status:  http.StatusOK,
		buf:     buf,
	}
}

// Header returns the header map of the underlying ResponseWriter.
func (r *ResponseRecorder) Header() http.Header {
	return r.wrapped.Header()
}

// WriteHeader captures status and delegates to the underlying ResponseWriter.
// Only the first call takes effect; subsequent calls are no-ops, matching
// the contract documented in net/http.ResponseWriter.
func (r *ResponseRecorder) WriteHeader(status int) {
	if r.headerWritten {
		return
	}
	r.headerWritten = true
	r.status = status
	r.wrapped.WriteHeader(status)
}

// Write tees p to both the internal buffer and the underlying ResponseWriter.
// If WriteHeader has not been called yet, it implicitly fires a 200 OK first,
// mirroring the behaviour of net/http's own ResponseWriter so that Status()
// always reflects the true status code sent on the wire.
func (r *ResponseRecorder) Write(p []byte) (int, error) {
	if !r.headerWritten {
		r.WriteHeader(http.StatusOK)
	}
	r.buf.Write(p) // bytes.Buffer.Write never returns an error
	return r.wrapped.Write(p)
}

// Status returns the HTTP status code captured by WriteHeader.
// It returns 200 if WriteHeader was never called.
func (r *ResponseRecorder) Status() int {
	return r.status
}

// Body returns the full response body written so far as a byte slice.
// The slice is only valid until Free is called.
func (r *ResponseRecorder) Body() []byte {
	return r.buf.Bytes()
}

// Free returns the internal buffer to the pool. It must be called exactly once
// after the ResponseRecorder is no longer needed (typically via defer).
// Accessing Body() after Free returns undefined results.
func (r *ResponseRecorder) Free() {
	bufPool.Put(r.buf)
	r.buf = nil
}
