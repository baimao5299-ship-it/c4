package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientReusesTransport(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	for i := 0; i < 3; i++ {
		resp, err := c.Get(srv.URL)
		require.NoError(t, err)
		resp.Body.Close()
	}
	require.Equal(t, 3, hits)
}
