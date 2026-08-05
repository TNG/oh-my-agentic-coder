package buildbroker

import (
	"encoding/base64"
	"io"
	"sync"
)

// frameWriter is the single serialized NDJSON frame writer the broker
// uses on an execute connection. It wraps the HTTP response writer,
// holds a mutex so concurrent stdout/stderr writers always produce
// valid whole NDJSON frames, and exposes a flush after each frame.
//
// frameWriter is also the io.Writer the engine invoker receives as
// stdout/stderr: each Write is chunked into MaxOutputFrameBytes-sized
// output frames and submitted through the locked frame writer, so
// byte order is preserved per stream and cross-stream ordering is
// best-effort (the mutex serializes whole frames).
type frameWriter struct {
	mu     sync.Mutex
	w      io.Writer
	flush  func() error
	closed bool
}

// newFrameWriter wraps w (the HTTP response writer) and flush (the
// response writer's Flush, or a no-op when not a flusher). The broker
// creates one per execute connection.
func newFrameWriter(w io.Writer, flush func() error) *frameWriter {
	if flush == nil {
		flush = func() error { return nil }
	}
	return &frameWriter{w: w, flush: flush}
}

// writeFrame writes one NDJSON frame and flushes. It is the single
// serialized entry point: concurrent callers (stdout writer, stderr
// writer, the result framer) all go through this under the mutex.
func (f *frameWriter) writeFrame(v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errClosed
	}
	if err := encodeFrame(f.w, v); err != nil {
		f.closed = true
		return err
	}
	if err := f.flush(); err != nil {
		f.closed = true
		return err
	}
	return nil
}

// writeAccepted writes the accepted frame.
func (f *frameWriter) writeAccepted(requestID string) error {
	return f.writeFrame(acceptedFrame{Type: string(frameTypeAccepted), RequestID: requestID})
}

// writeOutput writes one output frame for the given stream, carrying
// up to MaxOutputFrameBytes of raw bytes (the caller chunks larger
// writes). data is base64-encoded so arbitrary bytes and invalid UTF-8
// preserve the direct-writer contract.
func (f *frameWriter) writeOutput(stream outputStream, data []byte) error {
	return f.writeFrame(outputFrame{
		Type:       string(frameTypeOutput),
		Stream:     string(stream),
		DataBase64: base64.StdEncoding.EncodeToString(data),
	})
}

// writeResult writes the terminal result frame. The broker calls this
// exactly once per accepted request, after cleanup, whenever the
// response remains writable.
func (f *frameWriter) writeResult(class string, exitCode int, message string) error {
	return f.writeFrame(resultFrame{
		Type:     string(frameTypeResult),
		Class:    class,
		ExitCode: exitCode,
		Message:  message,
	})
}

// close marks the writer closed so further writeFrame calls return
// errClosed without touching the underlying writer. The broker calls
// this after the terminal result; a disconnected client is the only
// case in which writeResult returns an error (the underlying write
// fails), and the broker marks the writer closed at that point too.
func (f *frameWriter) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// errClosed is returned by writeFrame when the writer is closed (the
// terminal result has been written or the connection died).
var errClosed = io.ErrClosedPipe

// chunkedWriter is the io.Writer the engine invoker receives as
// stdout or stderr. Each Write is split into MaxOutputFrameBytes-sized
// chunks and submitted to the frameWriter as output frames on the
// given stream. Byte order is preserved per stream because Writes are
// serialized through the frameWriter's mutex.
type chunkedWriter struct {
	fw     *frameWriter
	stream outputStream
}

// newChunkedWriter returns an io.Writer that streams to stream through
// fw, chunking writes into MaxOutputFrameBytes-sized output frames.
func newChunkedWriter(fw *frameWriter, stream outputStream) io.Writer {
	return &chunkedWriter{fw: fw, stream: stream}
}

func (c *chunkedWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxOutputFrameBytes {
			n = MaxOutputFrameBytes
		}
		chunk := p[:n]
		if err := c.fw.writeOutput(c.stream, chunk); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
