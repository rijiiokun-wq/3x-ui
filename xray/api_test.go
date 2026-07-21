package xray

import (
	"context"
	"strings"
	"testing"

	statsService "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

type fakeStatsServiceClient struct {
	statsService.StatsServiceClient
	response *statsService.QueryStatsResponse
	err      error
}

func (f *fakeStatsServiceClient) QueryStats(
	context.Context,
	*statsService.QueryStatsRequest,
	...grpc.CallOption,
) (*statsService.QueryStatsResponse, error) {
	return f.response, f.err
}

func xrayAPIWithFakeStats(response *statsService.QueryStatsResponse) (*XrayAPI, *fakeStatsServiceClient) {
	fake := &fakeStatsServiceClient{response: response}
	var client statsService.StatsServiceClient = fake
	return &XrayAPI{
		StatsServiceClient: &client,
		StatsLastValues:    map[string]int64{},
		grpcClient:         &grpc.ClientConn{},
	}, fake
}

func TestGetRequiredUserString_Present(t *testing.T) {
	user := map[string]any{"email": "alice@example.com"}
	got, err := getRequiredUserString(user, "email")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "alice@example.com" {
		t.Fatalf("got %q, want %q", got, "alice@example.com")
	}
}

func TestGetRequiredUserString_Missing(t *testing.T) {
	user := map[string]any{}
	if _, err := getRequiredUserString(user, "email"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestGetRequiredUserString_NilValue(t *testing.T) {
	user := map[string]any{"email": nil}
	if _, err := getRequiredUserString(user, "email"); err == nil {
		t.Fatal("expected error for nil value")
	}
}

func TestGetRequiredUserString_WrongType(t *testing.T) {
	user := map[string]any{"email": 42}
	_, err := getRequiredUserString(user, "email")
	if err == nil {
		t.Fatal("expected error for non-string value")
	}
	if !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("expected %q in error, got: %v", "invalid type", err)
	}
}

func TestGetOptionalUserString_Present(t *testing.T) {
	user := map[string]any{"flow": "xtls-rprx-vision"}
	got, err := getOptionalUserString(user, "flow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "xtls-rprx-vision" {
		t.Fatalf("got %q, want %q", got, "xtls-rprx-vision")
	}
}

func TestGetOptionalUserString_MissingReturnsEmptyNoError(t *testing.T) {
	user := map[string]any{}
	got, err := getOptionalUserString(user, "flow")
	if err != nil {
		t.Fatalf("unexpected error for missing optional field: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestGetOptionalUserString_NilReturnsEmptyNoError(t *testing.T) {
	user := map[string]any{"flow": nil}
	got, err := getOptionalUserString(user, "flow")
	if err != nil {
		t.Fatalf("unexpected error for nil optional field: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestGetOptionalUserString_WrongTypeErrors(t *testing.T) {
	user := map[string]any{"flow": []string{"a", "b"}}
	if _, err := getOptionalUserString(user, "flow"); err == nil {
		t.Fatal("expected error for non-string optional value")
	}
}

func TestAdvanceClientTrafficBaselinesCommittedBoundaryCountsOnlyNewBytes(t *testing.T) {
	const email = "cycle-boundary@example.test"
	uplink := "user>>>" + email + ">>>traffic>>>uplink"
	downlink := "user>>>" + email + ">>>traffic>>>downlink"
	foreign := "user>>>other@example.test>>>traffic>>>uplink"
	api, fake := xrayAPIWithFakeStats(&statsService.QueryStatsResponse{Stat: []*statsService.Stat{
		{Name: uplink, Value: 25},
		{Name: downlink, Value: 40},
		{Name: foreign, Value: 999},
	}})
	api.StatsLastValues[uplink] = 10
	api.StatsLastValues[downlink] = 20
	api.StatsLastValues[foreign] = 7

	restore, err := api.AdvanceClientTrafficBaselines([]string{email})
	if err != nil {
		t.Fatalf("AdvanceClientTrafficBaselines: %v", err)
	}
	if restore == nil {
		t.Fatal("missing rollback closure")
	}
	// A committed reset deliberately does not invoke restore.
	if api.StatsLastValues[uplink] != 25 || api.StatsLastValues[downlink] != 40 {
		t.Fatalf("boundary baseline = %#v", api.StatsLastValues)
	}
	if api.StatsLastValues[foreign] != 7 {
		t.Fatalf("foreign baseline changed: %d", api.StatsLastValues[foreign])
	}

	fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{
		{Name: uplink, Value: 30},
		{Name: downlink, Value: 43},
		{Name: foreign, Value: 1_005},
	}}
	_, clients, err := api.GetTraffic()
	if err != nil {
		t.Fatalf("GetTraffic after committed boundary: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("client deltas = %+v", clients)
	}
	var target *ClientTraffic
	for _, traffic := range clients {
		if traffic.Email == email {
			target = traffic
		}
	}
	if target == nil || target.Up != 5 || target.Down != 3 {
		t.Fatalf("post-boundary target delta = %+v, want up=5 down=3", target)
	}
}

func TestAdvanceClientTrafficBaselinesRestorePreservesPresenceAndForeignState(t *testing.T) {
	const email = "rollback-boundary@example.test"
	uplink := "user>>>" + email + ">>>traffic>>>uplink"
	downlink := "user>>>" + email + ">>>traffic>>>downlink"
	foreign := "user>>>other@example.test>>>traffic>>>uplink"
	api, _ := xrayAPIWithFakeStats(&statsService.QueryStatsResponse{Stat: []*statsService.Stat{
		{Name: uplink, Value: 25},
		{Name: foreign, Value: 999},
	}})
	api.StatsLastValues[uplink] = 10
	api.StatsLastValues[foreign] = 7

	restore, err := api.AdvanceClientTrafficBaselines([]string{email})
	if err != nil {
		t.Fatalf("AdvanceClientTrafficBaselines: %v", err)
	}
	if api.StatsLastValues[uplink] != 25 || api.StatsLastValues[downlink] != 0 {
		t.Fatalf("advanced baselines = %#v", api.StatsLastValues)
	}
	restore()
	restore() // rollback is idempotent
	if api.StatsLastValues[uplink] != 10 {
		t.Fatalf("uplink baseline after restore = %d", api.StatsLastValues[uplink])
	}
	if _, exists := api.StatsLastValues[downlink]; exists {
		t.Fatal("missing downlink baseline was not restored to absence")
	}
	if api.StatsLastValues[foreign] != 7 {
		t.Fatalf("foreign baseline changed: %d", api.StatsLastValues[foreign])
	}
}
