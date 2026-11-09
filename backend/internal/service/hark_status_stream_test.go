//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type harkStatusRecorder struct {
	*httptest.ResponseRecorder
	gate chan struct{}
	once sync.Once
}

func (w *harkStatusRecorder) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "hark.status") {
		w.once.Do(func() { close(w.gate) })
	}
	return w.ResponseRecorder.Write(b)
}
func (w *harkStatusRecorder) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestHarkStatusBypassesContentBufferWithoutStartingTTFT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := make(chan struct{})
	w := &harkStatusRecorder{ResponseRecorder: httptest.NewRecorder(), gate: gate}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	read, write := io.Pipe()
	go func() {
		defer write.Close()
		io.WriteString(write, "event: hark.status\ndata: {\"type\":\"hark.status\",\"status\":\"processing\",\"line\":\"Reading fixture\"}\n\n")
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
			write.CloseWithError(errors.New("status buffered behind content"))
			return
		}
		time.Sleep(20 * time.Millisecond)
		io.WriteString(write, "data: {\"choices\":[{\"delta\":{\"content\":\"ORIGINAL_BODY\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\ndata: [DONE]\n\n")
	}()
	account := rawChatCompletionsTestAccount()
	account.Platform = PlatformHark
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	result, err := s.streamRawChatCompletions(c, &http.Response{Header: http.Header{}, Body: read}, account, "mapped-model", "mapped-model", "hark", nil, nil, time.Now(), openAISilentRefusalMinRequestBodyBytes+100)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.FirstTokenMs)
	require.GreaterOrEqual(t, *result.FirstTokenMs, 20)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Contains(t, w.Body.String(), "event: hark.status")
	require.Contains(t, w.Body.String(), "ORIGINAL_BODY")
}

func TestHarkStatusOnlyStreamNeverReplaysAcceptedTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	account := rawChatCompletionsTestAccount()
	account.Platform = PlatformHark
	s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	result, err := s.streamRawChatCompletions(c, &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader("event: hark.status\ndata: {\"type\":\"hark.status\",\"status\":\"processing\"}\n\n"))}, account, "mapped", "mapped", "hark", nil, nil, time.Now(), 100)
	require.Error(t, err)
	require.NotNil(t, result)
	var retry *UpstreamFailoverError
	require.False(t, errors.As(err, &retry))
	require.Nil(t, result.FirstTokenMs)
}
