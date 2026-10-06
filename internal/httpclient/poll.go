package httpclient

import (
	"net/http"

	"github.com/PiDmitrius/klax/internal/timing"
)

func Poll(client *http.Client) *http.Client {
	c := *client
	c.Timeout = timing.PollTimeout
	return &c
}
