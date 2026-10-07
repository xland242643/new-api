package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
		})
	}
}

func TestRelayErrorHandlerTruncatesInvalidJSONBodyInLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, "bad response status code 500", newAPIError.Error())
	require.Contains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), fmt.Sprintf("original_length=%d", len(body)))
	require.NotContains(t, logBuffer.String(), strings.Repeat("b", common.LocalLogContentLimit+1))
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerPreservesJSONAndSSEError(t *testing.T) {
	const message = "Input text data may contain inappropriate content."
	const payload = `{"error":{"code":"data_inspection_failed","param":null,"message":"` + message + `","type":"data_inspection_failed"},"id":"chatcmpl-example"}`
	testCases := []struct {
		name string
		body string
	}{
		{name: "JSON", body: payload},
		{name: "SSE LF", body: "data: " + payload + "\n\n"},
		{name: "SSE CR", body: "data:" + payload + "\r\r"},
		{name: "SSE at EOF", body: "data: " + payload},
		{
			name: "SSE multiline CRLF after non-error frames",
			body: "\xef\xbb\xbf: heartbeat\r\nevent: message\r\ndata: {\"message\":\"ordinary chunk\"}\r\n\r\n" +
				"data: not JSON\r\n\r\ndata: [DONE]\r\n\r\n" +
				"event: error\r\nid: example\r\ndata: {\"error\":{\"code\":\"data_inspection_failed\",\r\n" +
				"data: \"param\":null,\"message\":\"" + message + "\",\"type\":\"data_inspection_failed\"}}\r\n\r\n" +
				"data: {\"error\":{\"message\":\"later error\",\"code\":\"later_error\"}}\r\n\r\n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			newAPIError := RelayErrorHandler(context.Background(), resp, false)
			require.Equal(t, http.StatusBadRequest, newAPIError.StatusCode)
			require.Equal(t, types.ErrorCode("data_inspection_failed"), newAPIError.GetErrorCode())
			require.Equal(t, message, newAPIError.Error())
			openAIError := newAPIError.ToOpenAIError()
			require.Equal(t, "data_inspection_failed", openAIError.Code)
			require.Equal(t, "data_inspection_failed", openAIError.Type)
			require.Empty(t, openAIError.Param)
		})
	}
}

func TestRelayErrorHandlerSSEDoesNotEchoEnvelope(t *testing.T) {
	const privateText = "private-request-marker"
	body := `data: {"error":{"message":"upstream rejected input","type":"data_inspection_failed","code":"data_inspection_failed","metadata":{"request":"` + privateText + `"}},"request":"` + privateText + `"}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	newAPIError := RelayErrorHandler(context.Background(), resp, true)
	require.Equal(t, "upstream rejected input", newAPIError.Error())
	clientError, err := common.Marshal(newAPIError.ToOpenAIError())
	require.NoError(t, err)
	require.NotContains(t, string(clientError), privateText)
	require.Empty(t, newAPIError.Metadata)
}

func TestRelayErrorHandlerSSEAllowsLargeErrorLine(t *testing.T) {
	message := strings.Repeat("large error ", 7<<10)
	body := `data: {"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	newAPIError := RelayErrorHandler(context.Background(), resp, false)
	require.Equal(t, message, newAPIError.Error())
	require.Equal(t, types.ErrorCode("server_error"), newAPIError.GetErrorCode())
}

func TestRelayErrorHandlerInvalidSSEUsesStatusFallback(t *testing.T) {
	for _, body := range []string{
		"data: {\"error\":\"private-request-marker\"\n\n",
		"data: {\"message\":\"private-request-marker\"}\n\n",
	} {
		resp := &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		newAPIError := RelayErrorHandler(context.Background(), resp, true)
		require.Equal(t, http.StatusBadRequest, newAPIError.StatusCode)
		require.Equal(t, types.ErrorCodeBadResponseStatusCode, newAPIError.GetErrorCode())
		require.Equal(t, "bad response status code 400", newAPIError.Error())
		require.NotContains(t, newAPIError.ToOpenAIError().Message, "private-request-marker")
	}
}

func TestRelayErrorHandlerBoundsAndClosesErrorBody(t *testing.T) {
	// The read boundary cuts through a multi-byte rune. Oversized bodies must
	// still fall back without decoding or returning the truncated body.
	body := &trackedErrorBody{Reader: strings.NewReader(strings.Repeat("界", maxRelayErrorBodySize/3+2))}
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: body}
	newAPIError := RelayErrorHandler(context.Background(), resp, true)
	require.Equal(t, http.StatusBadRequest, newAPIError.StatusCode)
	require.Equal(t, types.ErrorCodeBadResponseStatusCode, newAPIError.GetErrorCode())
	require.Equal(t, "bad response status code 400", newAPIError.Error())
	require.Equal(t, int64(maxRelayErrorBodySize+1), body.Size()-int64(body.Len()))
	require.True(t, body.closed)
}

type trackedErrorBody struct {
	*strings.Reader
	closed bool
}

func (b *trackedErrorBody) Close() error {
	b.closed = true
	return nil
}

func TestRelayErrorHandlerKeepsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), body)
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}
