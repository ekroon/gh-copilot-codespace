package daemonproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Encoder writes daemonproto frames to an io.Writer using newline-delimited
// JSON. Concurrent calls to Write are NOT safe; callers must serialize
// access (e.g., behind a mutex) when multiple goroutines may produce frames.
//
// Encoder is intentionally a thin wrapper around json.Encoder so that frames
// can carry payloads larger than bufio.Scanner's default token size. The
// underlying json.Encoder writes a single newline after every frame, which is
// the framing the corresponding Decoder relies on (and is convenient for
// `tee` debugging on the wire).
type Encoder struct {
	w io.Writer
}

// NewEncoder constructs an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Write serializes the frame as JSON followed by a newline.
func (e *Encoder) Write(f Frame) error {
	data, err := MarshalFrame(f)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		written, writeErr := e.w.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if writeErr != nil {
			return fmt.Errorf("daemonproto: write frame: %w", writeErr)
		}
		if written == 0 {
			return fmt.Errorf("daemonproto: write frame: %w", io.ErrShortWrite)
		}
	}
	return nil
}

// MarshalFrame serializes one complete newline-delimited frame without
// writing it. Callers that need cancellation-aware writes can then track
// partial progress without risking interleaved JSON.
func MarshalFrame(f Frame) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("daemonproto: encode frame: %w", err)
	}
	return buf.Bytes(), nil
}

// Decoder reads daemonproto frames from an io.Reader. Like Encoder, it uses
// json.Decoder so it tolerates arbitrarily large frames. Decoder is not safe
// for concurrent use.
//
// Decoder returns io.EOF unwrapped when the stream ends cleanly between
// frames, so callers can drive it with a simple `for { f, err := dec.Read(); if
// errors.Is(err, io.EOF) { break } }` loop.
type Decoder struct {
	dec *json.Decoder
}

// NewDecoder constructs a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{dec: json.NewDecoder(r)}
}

// Read returns the next frame from the stream. Returns io.EOF unwrapped at
// end-of-stream, or a wrapped error if the next frame is malformed.
func (d *Decoder) Read() (Frame, error) {
	var f Frame
	if err := d.dec.Decode(&f); err != nil {
		if err == io.EOF {
			return Frame{}, io.EOF
		}
		return Frame{}, fmt.Errorf("daemonproto: decode frame: %w", err)
	}
	return f, nil
}
