package query

import (
	"bufio"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/lomik/pluto/pkg/config"
	"github.com/pkg/errors"
)

const (
	headerClickhouseQueryID = "X-ClickHouse-Query-Id"
	headerUserAgent         = "User-Agent"
	clientName              = "pluto/v0.1.0"
	defaultUser             = "default"
)

// Opts ...
type Opts struct {
	BufferSize int
	HTTPClient *http.Client
	QueryID    string
	Headers    map[string]string
	Discovery  func(ctx context.Context, dsn string) (string, error)
}

// Request ...
type Request struct {
	sync.Mutex
	ctx       context.Context
	ctxCancel context.CancelFunc
	writeMu   sync.Mutex
	writeDone bool // guarded by writeMu
	reader    *io.PipeReader
	writer    *io.PipeWriter
	writerBuf *bufio.Writer

	finished chan interface{}
	respErr  error     // read/write with mutex
	resp     *Response // read/write with mutex

	vars struct {
		reqBodyBytes int
	}
}

// Response ...
type Response struct {
	ctx       context.Context
	httpBody  io.ReadCloser
	closeOnce sync.Once
	closeErr  error

	vars struct {
		respBodyBytes int
	}
}

// New starts a new request to ClickHouse
func NewRequest(ctx context.Context, cfg config.ClickHouse, opts Opts) (*Request, error) {
	var err error
	dsn := cfg.DSN
	if opts.Discovery != nil {
		dsn, err = opts.Discovery(ctx, dsn)
		if err != nil {
			return nil, err
		}
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}

	// 1mb minimum size of buffer
	if opts.BufferSize < 1024*1024 {
		opts.BufferSize = 1024 * 1024
	}

	reader, writer := io.Pipe()

	writerBuf := bufio.NewWriterSize(writer, opts.BufferSize)

	ctx, ctxCancel := context.WithCancel(ctx)

	req := &Request{
		reader:    reader,
		writer:    writer,
		writerBuf: writerBuf,
		ctx:       ctx,
		ctxCancel: ctxCancel,
		finished:  make(chan interface{}),
	}

	url := &url.URL{
		Scheme:  u.Scheme,
		Host:    u.Host,
		Path:    u.Path,
		RawPath: u.RawPath,
	}
	values := url.Query()
	headers := http.Header{}
	headers.Set(headerUserAgent, clientName)

	uv := u.Query()
	// params from dsn
	for k := range uv {
		if strings.HasPrefix(strings.ToLower(k), "x-clickhouse-") {
			headers.Set(k, uv.Get(k))
		} else {
			values.Set(k, uv.Get(k))
		}
	}

	// params from separate config
	for k, v := range cfg.Params {
		if strings.HasPrefix(strings.ToLower(k), "x-clickhouse-") {
			headers.Set(k, v)
		} else {
			values.Set(k, v)
		}
	}

	if len(opts.QueryID) > 0 {
		headers.Set(headerClickhouseQueryID, opts.QueryID)
	}

	for k, v := range opts.Headers {
		headers.Set(k, v)
	}

	url.RawQuery = values.Encode()

	httpReq := (&http.Request{
		Method:           "POST",
		ProtoMajor:       1,
		ProtoMinor:       1,
		URL:              url,
		TransferEncoding: []string{"chunked"},
		Body:             req.reader,
		Header:           headers,
	}).WithContext(ctx)

	if u.User.Username() != "" {
		password, _ := u.User.Password()
		httpReq.SetBasicAuth(u.User.Username(), password)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	go func() {
		httpResp, err := httpClient.Do(httpReq)
		defer close(req.finished)

		if err != nil {
			req.Lock()
			req.respErr = err
			req.Unlock()
			req.reader.CloseWithError(err)
			return
		}

		if httpResp.StatusCode != http.StatusOK {
			// A server can reject the request without reading its body. Unblock
			// any Write/Flush before attempting to read the error response.
			req.reader.CloseWithError(fmt.Errorf("http status code %d", httpResp.StatusCode))
			body, bodyErr := io.ReadAll(httpResp.Body)
			closeErr := httpResp.Body.Close()

			if bodyErr != nil {
				err = fmt.Errorf("http status code %d, but can't read response body: %s", httpResp.StatusCode, bodyErr.Error())
			} else {
				err = fmt.Errorf("http status code %d: %s", httpResp.StatusCode, string(body))
			}
			err = stderrors.Join(err, closeErr)
			req.Lock()
			req.respErr = err
			req.Unlock()
			return
		}

		req.Lock()
		req.resp = &Response{ctx: ctx, httpBody: httpResp.Body}
		req.Unlock()
		// HTTP 200 headers can arrive before the upload completes. Keep the
		// pipe open until Finish or Close so full-duplex uploads can continue.
	}()

	return req, nil
}

// Write ...
func (req *Request) Write(p []byte) (int, error) {
	req.writeMu.Lock()
	defer req.writeMu.Unlock()
	if req.writeDone {
		return 0, io.ErrClosedPipe
	}
	n, err := req.writerBuf.Write(p)
	req.vars.reqBodyBytes += n
	if err != nil {
		req.Lock()
		respErr := req.respErr
		req.Unlock()
		if respErr != nil {
			return n, respErr
		}
	}
	return n, err
}

func (req *Request) WriteByte(b byte) error {
	req.writeMu.Lock()
	defer req.writeMu.Unlock()
	if req.writeDone {
		return io.ErrClosedPipe
	}
	err := req.writerBuf.WriteByte(b)
	if err == nil {
		req.vars.reqBodyBytes++
	} else {
		req.Lock()
		respErr := req.respErr
		req.Unlock()
		if respErr != nil {
			return respErr
		}
	}
	return err
}

// Close ...
func (req *Request) Close() error {
	req.ctxCancel()
	req.reader.CloseWithError(io.ErrClosedPipe)
	req.writer.CloseWithError(io.ErrClosedPipe)
	req.writeMu.Lock()
	req.writeDone = true
	req.writeMu.Unlock()

	<-req.finished

	req.Lock()
	err := req.respErr
	resp := req.resp
	req.Unlock()

	if err != nil {
		if resp != nil {
			resp.close(false)
		}
		return err
	}

	if resp != nil {
		return resp.close(false)
	}

	return nil
}

// Finish finishes the request and starts reading the response
func (req *Request) Finish() (*Response, error) {
	req.writeMu.Lock()
	defer req.writeMu.Unlock()
	if req.writeDone {
		return nil, io.ErrClosedPipe
	}
	req.writeDone = true

	if err := req.writerBuf.Flush(); err != nil {
		req.writer.CloseWithError(err)
		req.reader.CloseWithError(err)
		<-req.finished
		// may be an error from the server
		req.Lock()
		respErr := req.respErr
		resp := req.resp
		req.Unlock()
		if resp != nil {
			resp.close(false)
		}
		if respErr != nil {
			return nil, errors.WithStack(respErr)
		}
		return nil, errors.WithStack(err)
	}
	if err := req.writer.Close(); err != nil {
		<-req.finished
		// may be an error from the server
		req.Lock()
		respErr := req.respErr
		resp := req.resp
		req.Unlock()
		if resp != nil {
			resp.close(false)
		}
		if respErr != nil {
			return nil, errors.WithStack(respErr)
		}
		return nil, errors.WithStack(err)
	}

	<-req.finished

	req.Lock()
	err := req.respErr
	resp := req.resp
	req.Unlock()

	return resp, err
}

// Read reads data from the response
func (resp *Response) Read(p []byte) (int, error) {
	if resp == nil {
		return 0, fmt.Errorf("response is nil")
	}
	n, err := resp.httpBody.Read(p)

	resp.vars.respBodyBytes += n

	return n, err
}

// Close closes the response and discards any remaining body
func (resp *Response) Close() error {
	return resp.close(true)
}

func (resp *Response) close(drain bool) error {
	resp.closeOnce.Do(func() {
		var readErr error
		if drain {
			_, readErr = io.Copy(io.Discard, resp)
		}
		closeErr := resp.httpBody.Close()
		resp.closeErr = stderrors.Join(readErr, closeErr)
	})
	return resp.closeErr
}
