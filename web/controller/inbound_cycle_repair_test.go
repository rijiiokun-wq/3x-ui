package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	xuilogger "github.com/mhsanaei/3x-ui/v3/logger"
	"github.com/op/go-logging"
)

func TestRepairClientTrafficCyclesStrictJSONRejectsUnknownTrailingAndOversize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("XUI_LOG_FOLDER", t.TempDir())
	xuilogger.InitLogger(logging.ERROR)
	t.Cleanup(xuilogger.CloseLogger)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unknown field",
			body: `{"items":[],"unexpected":true}`,
		},
		{
			name: "nested unknown field",
			body: `{"items":[{"inboundId":1,"email":"a","uuid":"11111111-1111-4111-8111-111111111111","subId":"s","expectedSettings":{"expiryTime":1,"reset":0,"total":1,"enable":true,"unexpected":1},"expectedTraffic":{"expiryTime":1,"reset":0,"total":1,"enable":true,"up":0,"down":0},"target":{"expiryTime":2,"reset":0,"total":1},"resetTraffic":false}]}`,
		},
		{
			name: "trailing JSON",
			body: `{"items":[]} {"items":[]}`,
		},
		{
			name: "missing required zero-value field",
			body: `{"items":[{"inboundId":1,"email":"a","uuid":"11111111-1111-4111-8111-111111111111","subId":"s","expectedSettings":{"expiryTime":1,"reset":0,"total":1},"expectedTraffic":{"expiryTime":1,"reset":0,"total":1,"enable":false,"up":0,"down":0},"target":{"expiryTime":2,"reset":0,"total":1},"resetTraffic":false}]}`,
		},
		{
			name: "oversize",
			body: `{"items":[{"inboundId":1,"email":"` + strings.Repeat("x", maxRepairClientTrafficCyclesBodyBytes) + `"}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			controller := &InboundController{}
			router.POST("/:id/repairClientTrafficCycles", controller.repairClientTrafficCycles)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"/1/repairClientTrafficCycles",
				strings.NewReader(tc.body),
			)
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"success":false`) || !strings.Contains(body, "invalid_request") {
				t.Fatalf("unexpected response: %s", body)
			}
			if len(body) > 512 {
				t.Fatalf("error response leaked request details (%d bytes)", len(body))
			}
		})
	}
}
