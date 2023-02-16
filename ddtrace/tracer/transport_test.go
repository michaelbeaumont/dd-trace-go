// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getTestSpan returns a Span with different fields set
func getTestSpan() *span {
	return &span{
		TraceID:  42,
		SpanID:   52,
		ParentID: 42,
		Type:     "web",
		Service:  "high.throughput",
		Name:     "sending.events",
		Resource: "SEND /data",
		Start:    1481215590883401105,
		Duration: 1000000000,
		Meta:     map[string]string{"http.host": "192.168.0.1"},
		Metrics:  map[string]float64{"http.monitor": 41.99},
	}
}

// getTestTrace returns a list of traces that is composed by “traceN“ number
// of traces, each one composed by “size“ number of spans.
func getTestTrace(traceN, size int) [][]*span {
	var traces [][]*span

	for i := 0; i < traceN; i++ {
		trace := []*span{}
		for j := 0; j < size; j++ {
			trace = append(trace, getTestSpan())
		}
		traces = append(traces, trace)
	}
	return traces
}

func TestTracesAgentIntegration(t *testing.T) {
	if !integration {
		t.Skip("to enable integration test, set the INTEGRATION environment variable")
	}
	assert := assert.New(t)

	testCases := []struct {
		payload [][]*span
	}{
		{getTestTrace(1, 1)},
		{getTestTrace(10, 1)},
		{getTestTrace(1, 10)},
		{getTestTrace(10, 10)},
	}

	for _, tc := range testCases {
		transport := newHTTPTransport(defaultURL, defaultClient)
		p, err := encode(tc.payload)
		assert.NoError(err)
		_, err = transport.send(p)
		assert.NoError(err)
	}
}

func TestResolveAgentAddr(t *testing.T) {
	c := new(config)
	for _, tt := range []struct {
		inOpt            StartOption
		envHost, envPort string
		out              *url.URL
	}{
		{nil, "", "", &url.URL{Scheme: "http", Host: defaultAddress}},
		{nil, "ip.local", "", &url.URL{Scheme: "http", Host: fmt.Sprintf("ip.local:%s", defaultPort)}},
		{nil, "", "1234", &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:1234", defaultHostname)}},
		{nil, "ip.local", "1234", &url.URL{Scheme: "http", Host: "ip.local:1234"}},
		{WithAgentAddr("host:1243"), "", "", &url.URL{Scheme: "http", Host: "host:1243"}},
		{WithAgentAddr("ip.other:9876"), "ip.local", "", &url.URL{Scheme: "http", Host: "ip.other:9876"}},
		{WithAgentAddr("ip.other:1234"), "", "9876", &url.URL{Scheme: "http", Host: "ip.other:1234"}},
		{WithAgentAddr("ip.other:8888"), "ip.local", "1234", &url.URL{Scheme: "http", Host: "ip.other:8888"}},
	} {
		t.Run("", func(t *testing.T) {
			if tt.envHost != "" {
				os.Setenv("DD_AGENT_HOST", tt.envHost)
				defer os.Unsetenv("DD_AGENT_HOST")
			}
			if tt.envPort != "" {
				os.Setenv("DD_TRACE_AGENT_PORT", tt.envPort)
				defer os.Unsetenv("DD_TRACE_AGENT_PORT")
			}
			c.agentURL = resolveAgentAddr()
			if tt.inOpt != nil {
				tt.inOpt(c)
			}
			assert.Equal(t, tt.out, c.agentURL)
		})
	}

	t.Run("UDS", func(t *testing.T) {
		old := defaultSocketAPM
		d, err := os.Getwd()
		require.NoError(t, err)
		defaultSocketAPM = d // Choose a file we know will exist
		defer func() { defaultSocketAPM = old }()
		c.agentURL = resolveAgentAddr()
		assert.Equal(t, &url.URL{Scheme: "unix", Path: d}, c.agentURL)
	})
}

func TestTransportResponse(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
		err    string
	}{
		"ok": {
			status: http.StatusOK,
			body:   "Hello world!",
		},
		"bad": {
			status: http.StatusBadRequest,
			body:   strings.Repeat("X", 1002),
			err:    fmt.Sprintf("%s (Status: Bad Request)", strings.Repeat("X", 1000)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			ln, err := net.Listen("tcp4", ":0")
			assert.Nil(err)
			go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer ln.Close()
			url := "http://" + ln.Addr().String()
			transport := newHTTPTransport(url, defaultClient)
			rc, err := transport.send(newPayload())
			if tt.err != "" {
				assert.Equal(tt.err, err.Error())
				return
			}
			assert.NoError(err)
			slurp, err := io.ReadAll(rc)
			rc.Close()
			assert.NoError(err)
			assert.Equal(tt.body, string(slurp))
		})
	}
}

func TestTraceCountHeader(t *testing.T) {
	assert := assert.New(t)

	testCases := []struct {
		payload [][]*span
	}{
		{getTestTrace(1, 1)},
		{getTestTrace(10, 1)},
		{getTestTrace(100, 10)},
	}

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/info" {
			return
		}
		header := r.Header.Get("X-Datadog-Trace-Count")
		assert.NotEqual("", header, "X-Datadog-Trace-Count header should be here")
		count, err := strconv.Atoi(header)
		assert.Nil(err, "header should be an int")
		assert.NotEqual(0, count, "there should be a non-zero amount of traces")
	}))
	defer srv.Close()
	for _, tc := range testCases {
		transport := newHTTPTransport(srv.URL, defaultClient)
		p, err := encode(tc.payload)
		assert.NoError(err)
		_, err = transport.send(p)
		assert.NoError(err)
	}
	assert.Equal(hits, len(testCases))
}

type recordingRoundTripper struct {
	reqs []*http.Request
	rt   http.RoundTripper
}

// wrapRecordingRoundTripper wraps the client Transport with one that records all
// requests sent over the transport.
func wrapRecordingRoundTripper(client *http.Client) *recordingRoundTripper {
	rt := &recordingRoundTripper{rt: client.Transport}
	client.Transport = rt
	if rt.rt == nil {
		// Follow http.(*Client).Transport semantics.
		rt.rt = http.DefaultTransport
	}
	return rt
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, req)
	return r.rt.RoundTrip(req)
}

func TestCustomTransport(t *testing.T) {
	assert := assert.New(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	c := &http.Client{}
	crt := wrapRecordingRoundTripper(c)
	transport := newHTTPTransport(srv.URL, c)
	p, err := encode(getTestTrace(1, 1))
	assert.NoError(err)
	_, err = transport.send(p)
	assert.NoError(err)

	// make sure our custom round tripper was used
	assert.Len(crt.reqs, 1)
	assert.Equal(hits, 1)
}

// hitCounter returns an http handler function that counts the number of hits it receives. It also returns
// a wait function that blocks when called until:
// 1. the context passed into hitCounter times out
// 2. the expected number of hits is reached inside the handler function
// hitCounter is used to reduce flakiness of (TestWithHTTPClient, TestWithUDS) by clearly defining and waiting
// for an expected number of requests the tracer client will receive, and failing the test if this goal is
// not reached.
func hitCounter(ctx context.Context, t *testing.T, expectedHits int) (counter http.HandlerFunc, waiter func()) {
	received := make(chan struct{})
	var hits int
	return func(_ http.ResponseWriter, r *http.Request) {
			hits++
			if hits == expectedHits {
				received <- struct{}{}
			}
		}, func() {
			select {
			case <-ctx.Done():
				t.Fatalf("Time out: waiting on test server to receive (%d) hits", expectedHits)
			case <-received:
			}
		}
}

func TestWithHTTPClient(t *testing.T) {
	t.Setenv("DD_TRACE_STARTUP_LOGS", "0")

	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	expectedReqs := 3
	countReqs, waitForReqs := hitCounter(ctx, t, expectedReqs)

	srv := httptest.NewServer(countReqs)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	assert.NoError(err)
	c := &http.Client{}
	rt := wrapRecordingRoundTripper(c)
	trc := newTracer(WithAgentAddr(u.Host), WithHTTPClient(c))
	defer trc.Stop()

	p, err := encode(getTestTrace(1, 1))
	assert.NoError(err)
	_, err = trc.config.transport.send(p)
	assert.NoError(err)

	waitForReqs()

	assert.Len(rt.reqs, 3)
	assert.Contains(rt.reqs[0].URL.Path, "/info")
	assert.Contains(rt.reqs[1].URL.Path, "/traces")
	assert.Contains(rt.reqs[2].URL.Path, "/apmtelemetry")
}

func TestWithUDS(t *testing.T) {
	t.Setenv("DD_TRACE_STARTUP_LOGS", "0")

	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	dir, err := os.MkdirTemp("", "socket")
	if err != nil {
		t.Fatal(err)
	}
	udsPath := filepath.Join(dir, "apm.socket")
	defer os.RemoveAll(udsPath)
	unixListener, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatal(err)
	}

	expectedReqs := 3
	countHits, waitForReqs := hitCounter(ctx, t, expectedReqs)

	srv := http.Server{Handler: countHits}
	go srv.Serve(unixListener)
	defer srv.Close()

	trc := newTracer(WithUDS(udsPath))
	rt := wrapRecordingRoundTripper(trc.config.httpClient)
	defer trc.Stop()

	p, err := encode(getTestTrace(1, 1))
	assert.NoError(err)
	_, err = trc.config.transport.send(p)
	assert.NoError(err)

	waitForReqs()

	// There are 3 requests, but one happens on tracer startup before we wrap the round tripper.
	// This is OK for this test, since we just want to check that WithUDS allows communication
	// between a server and client over UDS. waitForReqs() tells us that there were 3 requests received.
	assert.Len(rt.reqs, expectedReqs-1)
}
