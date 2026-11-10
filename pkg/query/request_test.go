package query

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/lomik/pluto/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestRequestOK(t *testing.T) {
	// all ok
	t.Parallel()

	assert := assert.New(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.Equal("Hello, server", string(body))
		fmt.Fprint(w, "Hello, client")
	}))
	defer ts.Close()

	req, err := NewRequest(context.Background(),
		config.ClickHouse{
			DSN: fmt.Sprintf("http://%s", ts.Listener.Addr().String())},
		Opts{
			HTTPClient: ts.Client(),
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	resp, err := req.Finish()
	assert.NoError(err)

	respBody, err := io.ReadAll(resp)
	assert.Equal("Hello, client", string(respBody))
	assert.NoError(err)
}

func TestRequestWithHeaders(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	var receivedClientName string
	var receivedQueryID string
	var receivedDb string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		receivedClientName = r.Header.Get("User-Agent")
		receivedQueryID = r.Header.Get("X-Clickhouse-Query-Id")
		receivedDb = r.Header.Get("X-ClickHouse-Database")
		assert.NoError(err)
		assert.Equal("Hello, server", string(body))
		fmt.Fprint(w, "Hello, client")
	}))
	defer ts.Close()

	req, err := NewRequest(
		context.Background(),
		config.ClickHouse{
			DSN:    fmt.Sprintf("http://%s", ts.Listener.Addr().String()),
			Params: map[string]string{"X-ClickHouse-Database": "testdb"},
		},
		Opts{
			HTTPClient: ts.Client(),
			QueryID:    "query-id-test",
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	_, err = req.Finish()
	assert.NoError(err)

	assert.Equal("pluto/v0.1.0", receivedClientName)
	assert.Equal("query-id-test", receivedQueryID)
	assert.Equal("testdb", receivedDb)
}

func TestRequestResponseStatusNotOK(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.Equal("Hello, server", string(body))
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Hello, client")
	}))
	defer ts.Close()

	req, err := NewRequest(context.Background(),
		config.ClickHouse{DSN: fmt.Sprintf("http://%s", ts.Listener.Addr().String())},
		Opts{
			HTTPClient: ts.Client(),
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	resp, err := req.Finish()
	assert.Nil(resp)
	assert.Error(err)

	// просто для coverage
	_, err = io.ReadAll(resp)
	assert.Error(err)
}

func TestRequestBadRequest(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.Equal("Hello, server", string(body))
		fmt.Fprint(w, "Hello, client")
	}))
	defer ts.Close()

	req, err := NewRequest(context.Background(), config.ClickHouse{
		DSN: "http://",
	},
		Opts{
			// Addr: ts.Listener.Addr().String(),
			HTTPClient: ts.Client(),
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	resp, err := req.Finish()
	assert.Nil(resp)
	assert.Error(err)
}

func TestRequestAbortConnection(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	closeConnection := make(chan struct{})
	var closeOnce sync.Once
	releaseConnection := func() { closeOnce.Do(func() { close(closeConnection) }) }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		assert.NoError(err)
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "Hello, client")
		w.(http.Flusher).Flush()
		<-closeConnection
	}))
	defer ts.Close()
	defer releaseConnection()

	req, err := NewRequest(context.Background(),
		config.ClickHouse{DSN: fmt.Sprintf("http://%s", ts.Listener.Addr().String())},
		Opts{
			HTTPClient: ts.Client(),
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	resp, err := req.Finish()
	assert.NotNil(resp)
	assert.NoError(err)

	releaseConnection()
	respBody, err := io.ReadAll(resp)
	assert.Equal("Hello, client", string(respBody))
	assert.ErrorIs(err, io.ErrUnexpectedEOF)
}

func TestCancelByContext(t *testing.T) {
	t.Parallel()

	assert := assert.New(t)
	responseFlushed := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.Equal("Hello, server", string(body))

		flusher, ok := w.(http.Flusher)
		if !ok {
			panic("expected http.ResponseWriter to be an http.Flusher")
		}

		w.Header().Set("Connection", "Keep-Alive")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)

		fmt.Fprint(w, "Hello, client")
		flusher.Flush()
		close(responseFlushed)
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	req, err := NewRequest(ctx,
		config.ClickHouse{
			DSN: fmt.Sprintf("http://%s", ts.Listener.Addr().String()),
		},
		Opts{
			// HTTPClient:     ts.Client(),
		})
	assert.NoError(err)
	defer req.Close()

	_, err = fmt.Fprint(req, "Hello, server")
	assert.NoError(err)

	resp, err := req.Finish()
	assert.NoError(err)

	<-responseFlushed
	ctxCancel()
	_, err = io.ReadAll(resp)
	assert.Error(err)
}
