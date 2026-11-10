package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lomik/pluto/pkg/config"
	"github.com/stretchr/testify/require"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackingBody struct {
	io.ReadCloser
	closes atomic.Int32
	reads  atomic.Int64
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.reads.Add(int64(n))
	return n, err
}

func (b *trackingBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func trackingClient(ts *httptest.Server, tracked chan<- *trackingBody) *http.Client {
	client := ts.Client()
	transport := client.Transport
	client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(r)
		if err == nil {
			body := &trackingBody{ReadCloser: resp.Body}
			resp.Body = body
			tracked <- body
		}
		return resp, err
	})
	return client
}

func TestDiscoveryRequestURLParamsHeadersAndAuth(t *testing.T) {
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "request sent to original DSN", http.StatusTeapot)
	}))
	defer original.Close()

	type received struct {
		method, path, query, body, user, password string
		headers                                   http.Header
		authOK                                    bool
	}
	requests := make(chan received, 1)
	discovered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, password, ok := r.BasicAuth()
		requests <- received{r.Method, r.URL.Path, r.URL.RawQuery, string(body), user, password, r.Header.Clone(), ok}
		fmt.Fprint(w, "query result")
	}))
	defer discovered.Close()

	initialDSN := original.URL + "/wrong?database=original"
	var discoveryCalls int
	req, err := NewRequest(context.Background(), config.ClickHouse{
		DSN: initialDSN,
		Params: map[string]string{
			"database":              "config-db",
			"X-ClickHouse-Database": "config-header-db",
			"additional":            "config-value",
		},
	}, Opts{
		HTTPClient: discovered.Client(),
		Discovery: func(_ context.Context, dsn string) (string, error) {
			discoveryCalls++
			require.Equal(t, initialDSN, dsn)
			return strings.Replace(discovered.URL, "http://", "http://test%2Buser:p%40ss@", 1) +
				"/custom/path?database=dsn-db&query=dsn-query&X-ClickHouse-Database=dsn-header-db&x-clickhouse-token=dsn-token", nil
		},
		QueryID: "requested-id",
		Headers: map[string]string{
			"User-Agent":         "custom-agent",
			"X-ClickHouse-Token": "option-token",
		},
	})
	require.NoError(t, err)
	defer req.Close()
	require.Equal(t, 1, discoveryCalls)

	_, err = io.WriteString(req, "SELECT 1")
	require.NoError(t, err)
	resp, err := req.Finish()
	require.NoError(t, err)
	result, err := io.ReadAll(resp)
	require.NoError(t, err)
	require.Equal(t, "query result", string(result))

	got := <-requests
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/custom/path", got.path)
	require.Equal(t, "SELECT 1", got.body)
	require.True(t, got.authOK)
	require.Equal(t, "test+user", got.user)
	require.Equal(t, "p@ss", got.password)
	require.Equal(t, "custom-agent", got.headers.Get("User-Agent"))
	require.Equal(t, "requested-id", got.headers.Get("X-ClickHouse-Query-Id"))
	require.Equal(t, "config-header-db", got.headers.Get("X-ClickHouse-Database"))
	require.Equal(t, "option-token", got.headers.Get("X-ClickHouse-Token"))
	values, err := url.ParseQuery(got.query)
	require.NoError(t, err)
	require.Equal(t, "config-db", values.Get("database"))
	require.Equal(t, "dsn-query", values.Get("query"))
	require.Equal(t, "config-value", values.Get("additional"))
	require.NotContains(t, strings.ToLower(got.query), "x-clickhouse-")
}

func TestDiscoveryErrors(t *testing.T) {
	want := errors.New("discovery unavailable")
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://invalid"}, Opts{
		Discovery: func(context.Context, string) (string, error) { return "", want },
	})
	require.Nil(t, req)
	require.ErrorIs(t, err, want)

	req, err = NewRequest(context.Background(), config.ClickHouse{DSN: "http://valid"}, Opts{
		Discovery: func(context.Context, string) (string, error) { return "http://%invalid", nil },
	})
	require.Nil(t, req)
	require.Error(t, err)
}

func TestBaseDSNWithoutDiscovery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s %s", r.URL.Path, r.URL.Query().Get("mode"), r.Header.Get("X-ClickHouse-Database"), body)
	}))
	defer ts.Close()
	req, err := NewRequest(context.Background(), config.ClickHouse{
		DSN: ts.URL + "/base?mode=base&X-ClickHouse-Database=base-db",
	}, Opts{HTTPClient: ts.Client()})
	require.NoError(t, err)
	defer req.Close()
	_, err = io.WriteString(req, "query")
	require.NoError(t, err)
	resp, err := req.Finish()
	require.NoError(t, err)
	body, err := io.ReadAll(resp)
	require.NoError(t, err)
	require.Equal(t, "/base base base-db query", string(body))
}

func TestErrorStatusClosesResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "database error", http.StatusBadRequest)
	}))
	defer ts.Close()
	tracked := make(chan *trackingBody, 1)

	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: ts.URL}, Opts{
		HTTPClient: trackingClient(ts, tracked),
	})
	require.NoError(t, err)
	defer req.Close()
	_, err = io.WriteString(req, "broken SQL")
	require.NoError(t, err)
	resp, err := req.Finish()
	require.Nil(t, resp)
	require.ErrorContains(t, err, "http status code 400: database error")
	body := <-tracked
	require.EqualValues(t, 1, body.closes.Load())
	require.ErrorContains(t, req.Close(), "http status code 400: database error")
	require.EqualValues(t, 1, body.closes.Load())
}

func TestEarlyHTTPErrorStatus(t *testing.T) {
	responseSent := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not read r.Body: ClickHouse may reject the query before consuming
		// the upload. Flush the error response before returning.
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "too many queries")
		w.(http.Flusher).Flush()
		close(responseSent)
	}))
	defer ts.Close()

	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: ts.URL}, Opts{HTTPClient: ts.Client()})
	require.NoError(t, err)
	defer req.Close()
	<-responseSent
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(req, "SELECT from a table that is not there")
		if err == nil {
			_, err = req.Finish()
		}
		done <- err
	}()
	select {
	case err = <-done:
		// Depending on when net/http tears down the upload, the response
		// body can be either fully read or interrupted by connection close.
		require.ErrorContains(t, err, "http status code 429")
	case <-time.After(5 * time.Second):
		req.Close()
		<-done
		t.Fatal("real HTTP upload remained blocked after early response")
	}
}

func TestCloseCancelsPendingUpload(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	<-started
	done := make(chan error, 1)
	go func() {
		_, err := req.Write(make([]byte, 2*1024*1024))
		done <- err
	}()
	require.ErrorIs(t, req.Close(), context.Canceled)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		req.reader.CloseWithError(io.ErrClosedPipe)
		<-done
		t.Fatal("Close did not unblock Write")
	}
}

func TestCloseCancelsPendingFinish(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	<-started
	_, err = io.WriteString(req, "buffered query")
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() {
		_, err := req.Finish()
		finished <- err
	}()
	closed := make(chan error, 1)
	go func() { closed <- req.Close() }()
	select {
	case err := <-closed:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		req.reader.CloseWithError(io.ErrClosedPipe)
		t.Fatal("Close blocked behind a pending Finish")
	}
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Finish remained blocked after Close")
	}
}

func TestCloseAfterFinishCancelsUnreadResponse(t *testing.T) {
	serverDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "first chunk")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: ts.URL}, Opts{HTTPClient: ts.Client()})
	require.NoError(t, err)
	resp, err := req.Finish()
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NoError(t, req.Close())
	<-serverDone
}

func TestResponseCloseDrainsAndClosesBody(t *testing.T) {
	const result = "a longer response body"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, result)
	}))
	defer ts.Close()
	tracked := make(chan *trackingBody, 1)

	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: ts.URL}, Opts{
		HTTPClient: trackingClient(ts, tracked),
	})
	require.NoError(t, err)
	defer req.Close()
	resp, err := req.Finish()
	require.NoError(t, err)
	buf := make([]byte, 2)
	_, err = io.ReadFull(resp, buf)
	require.NoError(t, err)
	require.NoError(t, resp.Close())
	body := <-tracked
	require.EqualValues(t, len(result), body.reads.Load())
	require.EqualValues(t, 1, body.closes.Load())
	require.NoError(t, resp.Close())
	require.NoError(t, req.Close())
	require.EqualValues(t, 1, body.closes.Load())
}

type failingBody struct {
	err    error
	closes atomic.Int32
}

func (b *failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b *failingBody) Close() error {
	b.closes.Add(1)
	return nil
}

func TestResponseCloseAfterReadError(t *testing.T) {
	want := errors.New("response read failed")
	body := &failingBody{err: want}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	defer req.Close()
	resp, err := req.Finish()
	require.NoError(t, err)
	require.ErrorIs(t, resp.Close(), want)
	require.EqualValues(t, 1, body.closes.Load())
}

func TestResponseCloseReturnsUnderlyingCloseError(t *testing.T) {
	want := errors.New("response close failed")
	body := &errorBody{readErr: io.EOF, closeErr: want}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	resp, err := req.Finish()
	require.NoError(t, err)
	require.ErrorIs(t, resp.Close(), want)
	require.ErrorIs(t, req.Close(), want)
	require.EqualValues(t, 1, body.closes.Load())
}

func TestResponseCloseDuringRequestCancellation(t *testing.T) {
	var body *blockingResponseBody
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body = &blockingResponseBody{ctx: r.Context(), readStarted: make(chan struct{})}
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	resp, err := req.Finish()
	require.NoError(t, err)
	closed := make(chan error, 1)
	go func() { closed <- resp.Close() }()
	<-body.readStarted
	require.ErrorIs(t, req.Close(), context.Canceled)
	require.ErrorIs(t, <-closed, context.Canceled)
	require.EqualValues(t, 1, body.closes.Load())
}

type blockingResponseBody struct {
	ctx         context.Context
	readStarted chan struct{}
	readOnce    sync.Once
	closes      atomic.Int32
}

func (b *blockingResponseBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *blockingResponseBody) Close() error {
	b.closes.Add(1)
	return nil
}

// A transport may return response headers before consuming the request body.
// A bound here is intentional: failures to unblock io.Pipe must fail the test
// rather than hang the entire package test suite.
func awaitUpload(t *testing.T, req *Request, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		req.reader.CloseWithError(io.ErrClosedPipe)
		<-done
		t.Fatal("upload blocked after HTTP response")
		return nil
	}
}

func TestEarlyErrorResponseUnblocksUpload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*Request) error
	}{
		{"buffered Finish", func(req *Request) error {
			_, err := io.WriteString(req, "small buffered query")
			if err != nil {
				return err
			}
			_, err = req.Finish()
			return err
		}},
		{"large Write", func(req *Request) error {
			_, err := req.Write(make([]byte, 2*1024*1024))
			return err
		}},
		{"buffered WriteByte", func(req *Request) error {
			for i := 0; i < 2*1024*1024; i++ {
				if err := req.WriteByte('x'); err != nil {
					return err
				}
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader("early error")),
					Header:     make(http.Header),
				}, nil
			})}
			req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
			require.NoError(t, err)
			defer req.Close()
			<-req.finished
			done := make(chan error, 1)
			go func() { done <- tc.write(req) }()
			require.ErrorContains(t, awaitUpload(t, req, done), "http status code 400: early error")
		})
	}
}

func TestTransportErrorUnblocksUpload(t *testing.T) {
	want := errors.New("upstream unavailable")
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, want
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	defer req.Close()
	<-req.finished
	done := make(chan error, 1)
	go func() {
		_, err := req.Write(make([]byte, 2*1024*1024))
		done <- err
	}()
	require.ErrorIs(t, awaitUpload(t, req, done), want)
	_, err = req.Finish()
	require.ErrorIs(t, err, want)
}

func TestWritesAfterFinishAndClose(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	for _, tc := range []struct {
		name     string
		end      func(*Request) error
		cancelOK bool
	}{
		{"Finish", func(req *Request) error {
			resp, err := req.Finish()
			if err != nil {
				return err
			}
			return resp.Close()
		}, false},
		{"Close", func(req *Request) error { return req.Close() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := NewRequest(context.Background(), config.ClickHouse{DSN: ts.URL}, Opts{HTTPClient: ts.Client()})
			require.NoError(t, err)
			defer req.Close()
			endErr := tc.end(req)
			if tc.cancelOK && endErr != nil {
				require.ErrorIs(t, endErr, context.Canceled)
			} else {
				require.NoError(t, endErr)
			}
			_, err = io.WriteString(req, "no longer accepted")
			require.ErrorIs(t, err, io.ErrClosedPipe)
			require.ErrorIs(t, req.WriteByte('!'), io.ErrClosedPipe)
		})
	}
}

func TestErrorStatusBodyReadAndCloseErrors(t *testing.T) {
	readErr := errors.New("body read failed")
	closeErr := errors.New("body close failed")
	body := &errorBody{readErr: readErr, closeErr: closeErr}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: body, Header: make(http.Header)}, nil
	})}
	req, err := NewRequest(context.Background(), config.ClickHouse{DSN: "http://example.invalid"}, Opts{HTTPClient: client})
	require.NoError(t, err)
	defer req.Close()
	_, err = req.Finish()
	require.ErrorContains(t, err, "http status code 403, but can't read response body: body read failed")
	require.ErrorIs(t, err, closeErr)
	require.EqualValues(t, 1, body.closes.Load())
}

type errorBody struct {
	readErr, closeErr error
	closes            atomic.Int32
}

func (b *errorBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b *errorBody) Close() error {
	b.closes.Add(1)
	return b.closeErr
}
