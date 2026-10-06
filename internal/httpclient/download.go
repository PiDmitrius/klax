// Downloads expire after network inactivity; receiving bytes renews the budget.
package httpclient

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/PiDmitrius/klax/internal/timing"
)

func GetDownload(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return Download(client, req)
}

func Download(client *http.Client, req *http.Request) (*http.Response, error) {
	return download(client, req, timing.RequestTimeout)
}

func download(client *http.Client, req *http.Request, idle time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(idle, func() { cancel(context.DeadlineExceeded) })
	c := *client
	c.Timeout = 0
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	timer.Reset(idle)
	resp.Body = &downloadBody{ReadCloser: resp.Body, timer: timer, idle: idle, cancel: cancel}
	return resp, nil
}

type downloadBody struct {
	io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelCauseFunc
}

func (b *downloadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *downloadBody) Close() error {
	b.timer.Stop()
	b.cancel(nil)
	return b.ReadCloser.Close()
}
