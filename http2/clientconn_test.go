// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Infrastructure for testing ClientConn.RoundTrip.
// Put actual tests in transport_test.go.

package http2

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// TestTestClientConn demonstrates usage of testClientConn.
func TestTestClientConn(t *testing.T) {
	// newTestClientConn creates a *ClientConn and surrounding test infrastructure.
	tc := newTestClientConn(t)

	// tc.greet reads the client's initial SETTINGS and WINDOW_UPDATE frames,
	// and sends a SETTINGS frame to the client.
	//
	// Additional settings may be provided as optional parameters to greet.
	tc.greet()

	// Request bodies must either be constant (bytes.Buffer, strings.Reader)
	// or created with newRequestBody.
	body := tc.newRequestBody()
	body.writeBytes(10)         // 10 arbitrary bytes...
	body.closeWithError(io.EOF) // ...followed by EOF.

	// tc.roundTrip calls RoundTrip, but does not wait for it to return.
	// It returns a testRoundTrip.
	req, _ := http.NewRequest("PUT", "https://dummy.tld/", body)
	rt := tc.roundTrip(req)

	// tc has a number of methods to check for expected frames sent.
	// Here, we look for headers and the request body.
	tc.wantHeaders(wantHeader{
		streamID:  rt.streamID(),
		endStream: false,
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"PUT"},
			":path":      []string{"/"},
		},
	})
	// Expect 10 bytes of request body in DATA frames.
	tc.wantData(wantData{
		streamID:  rt.streamID(),
		endStream: true,
		size:      10,
	})

	// tc.writeHeaders sends a HEADERS frame back to the client.
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	// Now that we've received headers, RoundTrip has finished.
	// testRoundTrip has various methods to examine the response,
	// or to fetch the response and/or error returned by RoundTrip
	rt.wantStatus(200)
	rt.wantBody(nil)
}

// A testClientConn allows testing ClientConn.RoundTrip against a fake server.
//
// A test using testClientConn consists of:
//   - actions on the client (calling RoundTrip, making data available to Request.Body);
//   - validation of frames sent by the client to the server; and
//   - providing frames from the server to the client.
//
// testClientConn manages synchronization, so tests can generally be written as
// a linear sequence of actions and validations without additional synchronization.
type testClientConn struct {
	t *testing.T

	tr    *Transport
	fr    *Framer
	cc    *ClientConn
	hooks *testSyncHooks

	encbuf bytes.Buffer
	enc    *hpack.Encoder

	roundtrips []*testRoundTrip

	rerr error        // returned by Read
	rbuf bytes.Buffer // sent to the test conn
	wbuf bytes.Buffer // sent by the test conn
}

func newTestClientConn(t *testing.T, opts ...func(*Transport)) *testClientConn {
	t.Helper()

	tr := &Transport{}
	for _, o := range opts {
		o(tr)
	}

	tc := &testClientConn{
		t:     t,
		tr:    tr,
		hooks: newTestSyncHooks(),
	}
	tc.enc = hpack.NewEncoder(&tc.encbuf)
	tc.fr = NewFramer(&tc.rbuf, &tc.wbuf)
	tc.fr.ReadMetaHeaders = hpack.NewDecoder(initialHeaderTableSize, nil)
	tc.fr.SetMaxReadFrameSize(10 << 20)

	t.Cleanup(func() {
		if tc.rerr == nil {
			tc.rerr = io.EOF
		}
		tc.sync()
		if tc.hooks.total != 0 {
			t.Errorf("%v goroutines still running after test completed", tc.hooks.total)
		}

	})

	tc.hooks.newclientconn = func(cc *ClientConn) {
		tc.cc = cc
	}
	const singleUse = false
	_, err := tc.tr.newClientConn((*testClientConnNetConn)(tc), singleUse, tc.hooks)
	if err != nil {
		t.Fatal(err)
	}
	tc.sync()
	tc.hooks.newclientconn = nil

	// Read the client's HTTP/2 preface, sent prior to any HTTP/2 frames.
	buf := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(&tc.wbuf, buf); err != nil {
		t.Fatalf("reading preface: %v", err)
	}
	if !bytes.Equal(buf, clientPreface) {
		t.Fatalf("client preface: %q, want %q", buf, clientPreface)
	}

	return tc
}

// sync waits for the ClientConn under test to reach a stable state,
// with all goroutines blocked on some input.
func (tc *testClientConn) sync() {
	tc.hooks.waitInactive()
}

// advance advances synthetic time by a duration.
func (tc *testClientConn) advance(d time.Duration) {
	tc.hooks.advance(d)
	tc.sync()
}

// hasFrame reports whether a frame is available to be read.
func (tc *testClientConn) hasFrame() bool {
	return tc.wbuf.Len() > 0
}

// readFrame reads the next frame from the conn.
func (tc *testClientConn) readFrame() Frame {
	if tc.wbuf.Len() == 0 {
		return nil
	}
	fr, err := tc.fr.ReadFrame()
	if err != nil {
		return nil
	}
	return fr
}

// testClientConnReadFrame reads a frame of a specific type from the conn.
func testClientConnReadFrame[T any](tc *testClientConn) T {
	tc.t.Helper()
	var v T
	fr := tc.readFrame()
	if fr == nil {
		tc.t.Fatalf("got no frame, want frame %T", v)
	}
	v, ok := fr.(T)
	if !ok {
		tc.t.Fatalf("got frame %T, want %T", fr, v)
	}
	return v
}

// wantFrameType reads the next frame from the conn.
// It produces an error if the frame type is not the expected value.
func (tc *testClientConn) wantFrameType(want FrameType) {
	tc.t.Helper()
	fr := tc.readFrame()
	if fr == nil {
		tc.t.Fatalf("got no frame, want frame %v", want)
	}
	if got := fr.Header().Type; got != want {
		tc.t.Fatalf("got frame %v, want %v", got, want)
	}
}

type wantHeader struct {
	streamID  uint32
	endStream bool
	header    http.Header
}

// wantHeaders reads a HEADERS frame and potential CONTINUATION frames,
// and asserts that they contain the expected headers.
func (tc *testClientConn) wantHeaders(want wantHeader) {
	tc.t.Helper()
	got := testClientConnReadFrame[*MetaHeadersFrame](tc)
	if got, want := got.StreamID, want.streamID; got != want {
		tc.t.Fatalf("got stream ID %v, want %v", got, want)
	}
	if got, want := got.StreamEnded(), want.endStream; got != want {
		tc.t.Fatalf("got stream ended %v, want %v", got, want)
	}
	gotHeader := make(http.Header)
	for _, f := range got.Fields {
		gotHeader[f.Name] = append(gotHeader[f.Name], f.Value)
	}
	for k, v := range want.header {
		if !reflect.DeepEqual(v, gotHeader[k]) {
			tc.t.Fatalf("got header %q = %q; want %q", k, v, gotHeader[k])
		}
	}
}

type wantData struct {
	streamID  uint32
	endStream bool
	size      int
}

// wantData reads zero or more DATA frames, and asserts that they match the expectation.
func (tc *testClientConn) wantData(want wantData) {
	tc.t.Helper()
	gotSize := 0
	gotEndStream := false
	for tc.hasFrame() && !gotEndStream {
		data := testClientConnReadFrame[*DataFrame](tc)
		gotSize += len(data.Data())
		if data.StreamEnded() {
			gotEndStream = true
		}
	}
	if gotSize != want.size {
		tc.t.Fatalf("got %v bytes of DATA frames, want %v", gotSize, want.size)
	}
	if gotEndStream != want.endStream {
		tc.t.Fatalf("after %v bytes of DATA frames, got END_STREAM=%v; want %v", gotSize, gotEndStream, want.endStream)
	}
}

// testRequestBody is a Request.Body for use in tests.
type testRequestBody struct {
	tc *testClientConn

	// At most one of buf or bytes can be set at any given time:
	buf   bytes.Buffer // specific bytes to read from the body
	bytes int          // body contains this many arbitrary bytes

	err error // read error (comes after any available bytes)
}

func (tc *testClientConn) newRequestBody() *testRequestBody {
	b := &testRequestBody{
		tc: tc,
	}
	return b
}

// Read is called by the ClientConn to read from a request body.
func (b *testRequestBody) Read(p []byte) (n int, _ error) {
	b.tc.cc.syncHooks.blockUntil(func() bool {
		return b.buf.Len() > 0 || b.bytes > 0 || b.err != nil
	})
	switch {
	case b.buf.Len() > 0:
		return b.buf.Read(p)
	case b.bytes > 0:
		if len(p) > b.bytes {
			p = p[:b.bytes]
		}
		b.bytes -= len(p)
		for i := range p {
			p[i] = 'A'
		}
		return len(p), nil
	default:
		return 0, b.err
	}
}

// Close is called by the ClientConn when it is done reading from a request body.
func (b *testRequestBody) Close() error {
	return nil
}

// writeBytes adds n arbitrary bytes to the body.
func (b *testRequestBody) writeBytes(n int) {
	b.bytes += n
	b.checkWrite()
	b.tc.sync()
}

// Write adds bytes to the body.
func (b *testRequestBody) Write(p []byte) (int, error) {
	n, err := b.buf.Write(p)
	b.checkWrite()
	b.tc.sync()
	return n, err
}

func (b *testRequestBody) checkWrite() {
	if b.bytes > 0 && b.buf.Len() > 0 {
		b.tc.t.Fatalf("can't interleave Write and writeBytes on request body")
	}
	if b.err != nil {
		b.tc.t.Fatalf("can't write to request body after closeWithError")
	}
}

// closeWithError sets an error which will be returned by Read.
func (b *testRequestBody) closeWithError(err error) {
	b.err = err
	b.tc.sync()
}

// roundTrip starts a RoundTrip call.
//
// (Note that the RoundTrip won't complete until response headers are received,
// the request times out, or some other terminal condition is reached.)
func (tc *testClientConn) roundTrip(req *http.Request) *testRoundTrip {
	rt := &testRoundTrip{
		tc:    tc,
		donec: make(chan struct{}),
	}
	tc.roundtrips = append(tc.roundtrips, rt)
	tc.hooks.newstream = func(cs *clientStream) { rt.cs = cs }
	tc.cc.goRun(func() {
		defer close(rt.donec)
		rt.resp, rt.respErr = tc.cc.RoundTrip(req)
	})
	tc.sync()
	tc.hooks.newstream = nil

	tc.t.Cleanup(func() {
		res, _ := rt.result()
		if res != nil {
			res.Body.Close()
		}
	})

	return rt
}

func (tc *testClientConn) greet(settings ...Setting) {
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.writeSettings(settings...)
	tc.writeSettingsAck()
	tc.wantFrameType(FrameSettings) // acknowledgement
}

func (tc *testClientConn) writeSettings(settings ...Setting) {
	tc.t.Helper()
	if err := tc.fr.WriteSettings(settings...); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

func (tc *testClientConn) writeSettingsAck() {
	tc.t.Helper()
	if err := tc.fr.WriteSettingsAck(); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

func (tc *testClientConn) writeData(streamID uint32, endStream bool, data []byte) {
	tc.t.Helper()
	if err := tc.fr.WriteData(streamID, endStream, data); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

// makeHeaderBlockFragment encodes headers in a form suitable for inclusion
// in a HEADERS or CONTINUATION frame.
//
// It takes a list of alernating names and values.
func (tc *testClientConn) makeHeaderBlockFragment(s ...string) []byte {
	if len(s)%2 != 0 {
		tc.t.Fatalf("uneven list of header name/value pairs")
	}
	tc.encbuf.Reset()
	for i := 0; i < len(s); i += 2 {
		tc.enc.WriteField(hpack.HeaderField{Name: s[i], Value: s[i+1]})
	}
	return tc.encbuf.Bytes()
}

func (tc *testClientConn) writeHeaders(p HeadersFrameParam) {
	tc.t.Helper()
	if err := tc.fr.WriteHeaders(p); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

// writeHeadersMode writes header frames, as modified by mode:
//
//   - noHeader: Don't write the header.
//   - oneHeader: Write a single HEADERS frame.
//   - splitHeader: Write a HEADERS frame and CONTINUATION frame.
func (tc *testClientConn) writeHeadersMode(mode headerType, p HeadersFrameParam) {
	tc.t.Helper()
	switch mode {
	case noHeader:
	case oneHeader:
		tc.writeHeaders(p)
	case splitHeader:
		if len(p.BlockFragment) < 2 {
			panic("too small")
		}
		contData := p.BlockFragment[1:]
		contEnd := p.EndHeaders
		p.BlockFragment = p.BlockFragment[:1]
		p.EndHeaders = false
		tc.writeHeaders(p)
		tc.writeContinuation(p.StreamID, contEnd, contData)
	default:
		panic("bogus mode")
	}
}

func (tc *testClientConn) writeContinuation(streamID uint32, endHeaders bool, headerBlockFragment []byte) {
	tc.t.Helper()
	if err := tc.fr.WriteContinuation(streamID, endHeaders, headerBlockFragment); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

func (tc *testClientConn) writeGoAway(maxStreamID uint32, code ErrCode, debugData []byte) {
	tc.t.Helper()
	if err := tc.fr.WriteGoAway(maxStreamID, code, debugData); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

func (tc *testClientConn) writeWindowUpdate(streamID, incr uint32) {
	tc.t.Helper()
	if err := tc.fr.WriteWindowUpdate(streamID, incr); err != nil {
		tc.t.Fatal(err)
	}
	tc.sync()
}

// closeWrite causes the net.Conn used by the ClientConn to return a error
// from Read calls.
func (tc *testClientConn) closeWrite(err error) {
	tc.rerr = err
	tc.sync()
}

// testRoundTrip manages a RoundTrip in progress.
type testRoundTrip struct {
	tc      *testClientConn
	resp    *http.Response
	respErr error
	donec   chan struct{}
	cs      *clientStream
}

// streamID returns the HTTP/2 stream ID of the request.
func (rt *testRoundTrip) streamID() uint32 {
	return rt.cs.ID
}

// done reports whether RoundTrip has returned.
func (rt *testRoundTrip) done() bool {
	select {
	case <-rt.donec:
		return true
	default:
		return false
	}
}

// result returns the result of the RoundTrip.
func (rt *testRoundTrip) result() (*http.Response, error) {
	t := rt.tc.t
	t.Helper()
	select {
	case <-rt.donec:
	default:
		t.Fatalf("RoundTrip (stream %v) is not done; want it to be", rt.streamID())
	}
	return rt.resp, rt.respErr
}

// response returns the response of a successful RoundTrip.
// If the RoundTrip unexpectedly failed, it calls t.Fatal.
func (rt *testRoundTrip) response() *http.Response {
	t := rt.tc.t
	t.Helper()
	resp, err := rt.result()
	if err != nil {
		t.Fatalf("RoundTrip returned unexpected error: %v", rt.respErr)
	}
	if resp == nil {
		t.Fatalf("RoundTrip returned nil *Response and nil error")
	}
	return resp
}

// err returns the (possibly nil) error result of RoundTrip.
func (rt *testRoundTrip) err() error {
	t := rt.tc.t
	t.Helper()
	_, err := rt.result()
	return err
}

// wantStatus indicates the expected response StatusCode.
func (rt *testRoundTrip) wantStatus(want int) {
	t := rt.tc.t
	t.Helper()
	if got := rt.response().StatusCode; got != want {
		t.Fatalf("got response status %v, want %v", got, want)
	}
}

// body reads the contents of the response body.
func (rt *testRoundTrip) readBody() ([]byte, error) {
	t := rt.tc.t
	t.Helper()
	return io.ReadAll(rt.response().Body)
}

// wantBody indicates the expected response body.
// (Note that this consumes the body.)
func (rt *testRoundTrip) wantBody(want []byte) {
	t := rt.tc.t
	t.Helper()
	got, err := rt.readBody()
	if err != nil {
		t.Fatalf("unexpected error reading response body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected response body:\ngot:  %q\nwant: %q", got, want)
	}
}

// wantHeaders indicates the expected response headers.
func (rt *testRoundTrip) wantHeaders(want http.Header) {
	t := rt.tc.t
	t.Helper()
	res := rt.response()
	if diff := diffHeaders(res.Header, want); diff != "" {
		t.Fatalf("unexpected response headers:\n%v", diff)
	}
}

// wantTrailers indicates the expected response trailers.
func (rt *testRoundTrip) wantTrailers(want http.Header) {
	t := rt.tc.t
	t.Helper()
	res := rt.response()
	if diff := diffHeaders(res.Trailer, want); diff != "" {
		t.Fatalf("unexpected response trailers:\n%v", diff)
	}
}

func diffHeaders(got, want http.Header) string {
	// nil and 0-length non-nil are equal.
	if len(got) == 0 && len(want) == 0 {
		return ""
	}
	// We could do a more sophisticated diff here.
	// DeepEqual is good enough for now.
	if reflect.DeepEqual(got, want) {
		return ""
	}
	return fmt.Sprintf("got:  %v\nwant: %v", got, want)
}

// testClientConnNetConn implements net.Conn.
type testClientConnNetConn testClientConn

func (nc *testClientConnNetConn) Read(b []byte) (n int, err error) {
	nc.cc.syncHooks.blockUntil(func() bool {
		return nc.rerr != nil || nc.rbuf.Len() > 0
	})
	if nc.rbuf.Len() > 0 {
		return nc.rbuf.Read(b)
	}
	return 0, nc.rerr
}

func (nc *testClientConnNetConn) Write(b []byte) (n int, err error) {
	return nc.wbuf.Write(b)
}

func (*testClientConnNetConn) Close() error {
	return nil
}

func (*testClientConnNetConn) LocalAddr() (_ net.Addr)            { return }
func (*testClientConnNetConn) RemoteAddr() (_ net.Addr)           { return }
func (*testClientConnNetConn) SetDeadline(t time.Time) error      { return nil }
func (*testClientConnNetConn) SetReadDeadline(t time.Time) error  { return nil }
func (*testClientConnNetConn) SetWriteDeadline(t time.Time) error { return nil }