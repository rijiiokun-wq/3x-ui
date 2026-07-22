package service

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/database"
	"github.com/mhsanaei/3x-ui/v3/database/model"
	"github.com/mhsanaei/3x-ui/v3/xray"
)

const (
	cycleRepairEmailA = "component-a@example.test"
	cycleRepairEmailB = "component-b@example.test"
	cycleRepairUUIDA  = "A1111111-B111-4111-8111-111111111111"
	cycleRepairUUIDB  = "22222222-2222-4222-8222-222222222222"
	cycleRepairSubID  = "shared-subscription-id"
)

type cycleRepairFixture struct {
	inbound       model.Inbound
	settingsItems []map[string]any
	traffic       map[string]xray.ClientTraffic
	records       map[string]model.ClientRecord
}

func cycleRepairService() *InboundService {
	return &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		return func() {}, nil
	}}
}

func setupCycleRepairFixture(t *testing.T, startWriter bool) cycleRepairFixture {
	t.Helper()
	dbDir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	resetTrafficWriterForTest(t)
	if startWriter {
		StartTrafficWriter()
	}

	clients := []map[string]any{
		{
			"id": cycleRepairUUIDA, "email": cycleRepairEmailA, "subId": cycleRepairSubID,
			"expiryTime": int64(100_000), "reset": 30, "totalGB": int64(1_000), "enable": false,
			"flow": "xtls-rprx-vision", "customTargetField": map[string]any{"keep": true},
		},
		{
			"id": cycleRepairUUIDB, "email": cycleRepairEmailB, "subId": cycleRepairSubID,
			"expiryTime": int64(101_000), "reset": 0, "totalGB": int64(2_000), "enable": true,
			"flow": "", "customSiblingField": []any{"untouched", float64(7)},
		},
	}
	settingsBytes, err := json.Marshal(map[string]any{
		"clients": clients, "decryption": "none", "fallbacks": []any{},
		"unknownRoot": map[string]any{"preserve": "yes"},
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	inbound := model.Inbound{
		UserId: 1, Tag: "cycle-repair-vless", Remark: "keep inbound", Enable: true,
		Port: 42424, Protocol: model.VLESS, Total: 99_999,
		Settings:       string(settingsBytes),
		StreamSettings: `{"network":"ws","wsSettings":{"path":"/preserve"}}`,
		Sniffing:       `{"enabled":true,"destOverride":["http","tls"]}`,
	}
	if err := database.GetDB().Create(&inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}

	records := map[string]model.ClientRecord{
		cycleRepairEmailA: {
			Email: cycleRepairEmailA, UUID: cycleRepairUUIDA, SubID: cycleRepairSubID,
			ExpiryTime: 80_000, Reset: 7, TotalGB: 1_200, Enable: false,
			Group: "vip", Comment: "preserve record", UpdatedAt: 12_345,
		},
		cycleRepairEmailB: {
			Email: cycleRepairEmailB, UUID: cycleRepairUUIDB, SubID: cycleRepairSubID,
			ExpiryTime: 81_000, Reset: 14, TotalGB: 2_200, Enable: true,
			Group: "sibling", Comment: "untouched sibling", UpdatedAt: 23_456,
		},
	}
	for email, record := range records {
		record := record
		desiredEnable := record.Enable
		if err := database.GetDB().Create(&record).Error; err != nil {
			t.Fatalf("create client record %s: %v", email, err)
		}
		// ClientRecord has gorm:"default:true"; force the explicit false test
		// fixture after Create so settings/traffic/record drift is intentional.
		if !desiredEnable {
			if err := database.GetDB().Model(&model.ClientRecord{}).Where("id = ?", record.Id).
				UpdateColumn("enable", false).Error; err != nil {
				t.Fatalf("force disabled client record %s: %v", email, err)
			}
			if err := database.GetDB().First(&record, record.Id).Error; err != nil {
				t.Fatalf("reload disabled client record %s: %v", email, err)
			}
		}
		records[email] = record
		if err := database.GetDB().Create(&model.ClientInbound{ClientId: record.Id, InboundId: inbound.Id}).Error; err != nil {
			t.Fatalf("create client attachment %s: %v", email, err)
		}
	}

	traffic := map[string]xray.ClientTraffic{
		cycleRepairEmailA: {
			InboundId: inbound.Id, Email: cycleRepairEmailA, ExpiryTime: 90_000,
			Reset: 0, Total: 1_100, Enable: false, Up: 5, Down: 7, LastOnline: 777,
		},
		cycleRepairEmailB: {
			InboundId: inbound.Id, Email: cycleRepairEmailB, ExpiryTime: 91_000,
			Reset: 30, Total: 2_100, Enable: true, Up: 11, Down: 13, LastOnline: 888,
		},
	}
	for email, row := range traffic {
		row := row
		if err := database.GetDB().Create(&row).Error; err != nil {
			t.Fatalf("create traffic %s: %v", email, err)
		}
		traffic[email] = row
	}

	return cycleRepairFixture{inbound: inbound, settingsItems: clients, traffic: traffic, records: records}
}

func (f cycleRepairFixture) item(email string, targetExpiry int64, resetTraffic bool) ClientTrafficCycleRepairItem {
	settingsIndex := 0
	clientUUID := cycleRepairUUIDA
	if email == cycleRepairEmailB {
		settingsIndex = 1
		clientUUID = cycleRepairUUIDB
	}
	settings := f.settingsItems[settingsIndex]
	traffic := f.traffic[email]
	return ClientTrafficCycleRepairItem{
		InboundID: f.inbound.Id,
		Email:     email,
		UUID:      clientUUID,
		SubID:     cycleRepairSubID,
		ExpectedSettings: ClientTrafficCycleSettingsExpected{
			ExpiryTime: settings["expiryTime"].(int64),
			Reset:      settings["reset"].(int),
			Total:      settings["totalGB"].(int64),
			Enable:     settings["enable"].(bool),
		},
		ExpectedTraffic: ClientTrafficCycleTrafficExpected{
			ExpiryTime: traffic.ExpiryTime,
			Reset:      traffic.Reset,
			Total:      traffic.Total,
			Enable:     traffic.Enable,
			Up:         traffic.Up,
			Down:       traffic.Down,
		},
		Target:       ClientTrafficCycleTarget{ExpiryTime: targetExpiry, Reset: 30, Total: 1_500},
		ResetTraffic: resetTraffic,
	}
}

func TestRepairClientTrafficCyclesAtomicSharedSubIDPreservesSiblingsAndUnknownFields(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	beforeSettings := decodeCycleRepairTestJSON(t, f.inbound.Settings)
	siblingBefore := beforeSettings["clients"].([]any)[1]
	recordBefore := f.records[cycleRepairEmailA]
	siblingRecordBefore := f.records[cycleRepairEmailB]
	siblingTrafficBefore := f.traffic[cycleRepairEmailB]
	boundaryCalled := false
	boundaryRestored := false
	svc := &InboundService{trafficGenerationBoundary: func(emails []string) (func(), error) {
		boundaryCalled = len(emails) == 1 && emails[0] == cycleRepairEmailA
		return func() { boundaryRestored = true }, nil
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{f.item(cycleRepairEmailA, 200_000, true)},
	)
	if err != nil {
		t.Fatalf("RepairClientTrafficCycles: %v", err)
	}
	if !needRestart || !result.NeedRestart || result.Updated != 1 {
		t.Fatalf("result = %+v, needRestart=%v", result, needRestart)
	}
	if got := result.Items[0]; got.Status != "updated" || !got.TrafficReset || !got.Reenabled {
		t.Fatalf("item result = %+v", got)
	}
	if !boundaryCalled || boundaryRestored {
		t.Fatalf("committed runtime boundary: called=%v restored=%v", boundaryCalled, boundaryRestored)
	}

	var inboundAfter model.Inbound
	if err := database.GetDB().First(&inboundAfter, f.inbound.Id).Error; err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	afterSettings := decodeCycleRepairTestJSON(t, inboundAfter.Settings)
	afterClients := afterSettings["clients"].([]any)
	targetAfter := afterClients[0].(map[string]any)
	if targetAfter["expiryTime"] != float64(200_000) || targetAfter["reset"] != float64(30) ||
		targetAfter["totalGB"] != float64(1_500) || targetAfter["enable"] != true {
		t.Fatalf("target settings not repaired: %#v", targetAfter)
	}
	if targetAfter["customTargetField"].(map[string]any)["keep"] != true || targetAfter["subId"] != cycleRepairSubID {
		t.Fatalf("target unknown/identity fields changed: %#v", targetAfter)
	}
	if !reflect.DeepEqual(siblingBefore, afterClients[1]) {
		t.Fatalf("shared-subId sibling changed:\nbefore=%#v\nafter=%#v", siblingBefore, afterClients[1])
	}
	if !reflect.DeepEqual(beforeSettings["unknownRoot"], afterSettings["unknownRoot"]) ||
		!reflect.DeepEqual(beforeSettings["fallbacks"], afterSettings["fallbacks"]) {
		t.Fatalf("root settings changed outside clients: before=%#v after=%#v", beforeSettings, afterSettings)
	}
	if inboundAfter.StreamSettings != f.inbound.StreamSettings || inboundAfter.Sniffing != f.inbound.Sniffing ||
		inboundAfter.Tag != f.inbound.Tag || inboundAfter.Port != f.inbound.Port || inboundAfter.Total != f.inbound.Total {
		t.Fatalf("inbound/transport fields changed: before=%+v after=%+v", f.inbound, inboundAfter)
	}

	var trafficAfter xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&trafficAfter).Error; err != nil {
		t.Fatalf("reload target traffic: %v", err)
	}
	if trafficAfter.ExpiryTime != 200_000 || trafficAfter.Reset != 30 || trafficAfter.Total != 1_500 ||
		!trafficAfter.Enable || trafficAfter.Up != 0 || trafficAfter.Down != 0 || trafficAfter.LastOnline != 777 {
		t.Fatalf("target traffic = %+v", trafficAfter)
	}
	var siblingTraffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailB).First(&siblingTraffic).Error; err != nil {
		t.Fatalf("reload sibling traffic: %v", err)
	}
	if !reflect.DeepEqual(siblingTrafficBefore, siblingTraffic) {
		t.Fatalf("sibling traffic changed: before=%+v after=%+v", siblingTrafficBefore, siblingTraffic)
	}

	var recordAfter model.ClientRecord
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&recordAfter).Error; err != nil {
		t.Fatalf("reload target record: %v", err)
	}
	if recordAfter.ExpiryTime != 200_000 || recordAfter.Reset != 30 || recordAfter.TotalGB != 1_500 || !recordAfter.Enable {
		t.Fatalf("target record = %+v", recordAfter)
	}
	if recordAfter.UpdatedAt != recordBefore.UpdatedAt || recordAfter.Group != recordBefore.Group ||
		recordAfter.Comment != recordBefore.Comment || recordAfter.UUID != recordBefore.UUID || recordAfter.SubID != recordBefore.SubID {
		t.Fatalf("record metadata/identity changed: before=%+v after=%+v", recordBefore, recordAfter)
	}
	var siblingRecord model.ClientRecord
	if err := database.GetDB().Where("email = ?", cycleRepairEmailB).First(&siblingRecord).Error; err != nil {
		t.Fatalf("reload sibling record: %v", err)
	}
	if !reflect.DeepEqual(siblingRecordBefore, siblingRecord) {
		t.Fatalf("sibling record changed: before=%+v after=%+v", siblingRecordBefore, siblingRecord)
	}
}

func TestRepairClientTrafficCyclesStaleItemRollsBackWholeBatch(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	settingsBefore := f.inbound.Settings
	trafficBefore := f.traffic[cycleRepairEmailA]
	recordBefore := f.records[cycleRepairEmailA]
	first := f.item(cycleRepairEmailA, 200_000, true)
	second := f.item(cycleRepairEmailB, 201_000, false)
	second.ExpectedTraffic.Up++
	boundaryCalled := false
	svc := &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		boundaryCalled = true
		return func() {}, nil
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{first, second},
	)
	if err == nil {
		t.Fatal("stale batch unexpectedly succeeded")
	}
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Index != 1 || repairErr.Code != "stale_traffic_precondition" {
		t.Fatalf("error = %#v", err)
	}
	if needRestart || result.Updated != 0 || result.NeedRestart {
		t.Fatalf("rejected result = %+v, needRestart=%v", result, needRestart)
	}
	if result.Items[0].Status != "not_applied" || result.Items[1].Status != "rejected" {
		t.Fatalf("item statuses = %+v", result.Items)
	}
	if boundaryCalled {
		t.Fatal("runtime boundary advanced before full batch validation")
	}
	assertCycleRepairStateUnchanged(t, f.inbound.Id, settingsBefore, trafficBefore, recordBefore)
}

func TestRepairClientTrafficCyclesWriteFailureRollsBackEarlierItem(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	settingsBefore := f.inbound.Settings
	trafficBefore := f.traffic[cycleRepairEmailA]
	recordBefore := f.records[cycleRepairEmailA]

	// Both items pass the complete read/validation phase. Fail the second
	// ClientRecord write so the transaction must undo the first item's traffic
	// and ClientRecord writes as well as the second item's traffic write.
	if err := database.GetDB().Exec(`
		CREATE TRIGGER fail_second_cycle_repair_record
		BEFORE UPDATE ON clients
		WHEN OLD.email = '` + cycleRepairEmailB + `'
		BEGIN
			SELECT RAISE(ABORT, 'forced cycle repair write failure');
		END
	`).Error; err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	boundaryBaseline := int64(10)
	boundaryCalled := false
	boundaryRestored := false
	svc := &InboundService{trafficGenerationBoundary: func(emails []string) (func(), error) {
		boundaryCalled = true
		old := boundaryBaseline
		boundaryBaseline = 25
		return func() {
			boundaryRestored = true
			boundaryBaseline = old
		}, nil
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{
			f.item(cycleRepairEmailA, 200_000, true),
			f.item(cycleRepairEmailB, 201_000, false),
		},
	)
	if err == nil {
		t.Fatal("write-failure batch unexpectedly succeeded")
	}
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Index != 1 || repairErr.Code != "client_record_write_failed" {
		t.Fatalf("error = %#v", err)
	}
	if needRestart || result.Updated != 0 || result.NeedRestart {
		t.Fatalf("rolled-back result = %+v, needRestart=%v", result, needRestart)
	}
	if result.Items[0].Status != "not_applied" || result.Items[0].Reason != "batch_rolled_back" ||
		result.Items[1].Status != "rejected" {
		t.Fatalf("item statuses = %+v", result.Items)
	}
	if !boundaryCalled || !boundaryRestored || boundaryBaseline != 10 {
		t.Fatalf("runtime boundary rollback: called=%v restored=%v baseline=%d", boundaryCalled, boundaryRestored, boundaryBaseline)
	}
	assertCycleRepairStateUnchanged(t, f.inbound.Id, settingsBefore, trafficBefore, recordBefore)
}

func TestRepairClientTrafficCyclesRequiresCanonicalVLESSIDField(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	settings := decodeCycleRepairTestJSON(t, f.inbound.Settings)
	client := settings["clients"].([]any)[0].(map[string]any)
	client["uuid"] = client["id"]
	delete(client, "id")
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal malformed settings: %v", err)
	}
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", f.inbound.Id).
		Update("settings", string(encoded)).Error; err != nil {
		t.Fatalf("store malformed settings: %v", err)
	}

	_, _, err = cycleRepairService().RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{f.item(cycleRepairEmailA, 200_000, true)},
	)
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "settings_identity_mismatch" {
		t.Fatalf("error = %v", err)
	}
}

func TestRepairClientTrafficCyclesWithoutResetPreservesCountersAndEnable(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	item := f.item(cycleRepairEmailA, 200_000, false)
	boundaryCalled := false
	svc := &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		boundaryCalled = true
		return func() {}, nil
	}}
	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{item},
	)
	if err != nil {
		t.Fatalf("RepairClientTrafficCycles: %v", err)
	}
	if needRestart || result.NeedRestart || result.Items[0].TrafficReset || result.Items[0].Reenabled {
		t.Fatalf("unexpected reset/restart result: %+v needRestart=%v", result, needRestart)
	}
	if boundaryCalled {
		t.Fatal("resetTraffic=false unexpectedly advanced runtime baseline")
	}
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&traffic).Error; err != nil {
		t.Fatalf("reload traffic: %v", err)
	}
	if traffic.Up != 5 || traffic.Down != 7 || traffic.Enable {
		t.Fatalf("reset=false changed counters/enable: %+v", traffic)
	}
	var record model.ClientRecord
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&record).Error; err != nil {
		t.Fatalf("reload record: %v", err)
	}
	if record.Enable {
		t.Fatalf("reset=false re-enabled ClientRecord: %+v", record)
	}
	settings := decodeCycleRepairTestJSON(t, mustCycleRepairInboundSettings(t, f.inbound.Id))
	client := settings["clients"].([]any)[0].(map[string]any)
	if client["enable"] != false {
		t.Fatalf("reset=false changed settings enable: %#v", client)
	}
}

func TestRepairClientTrafficCyclesWithoutResetAcceptsMonotonicCounterGrowth(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	item := f.item(cycleRepairEmailA, 200_000, false)
	if err := database.GetDB().Model(&xray.ClientTraffic{}).
		Where("inbound_id = ? AND email = ?", f.inbound.Id, cycleRepairEmailA).
		UpdateColumns(map[string]any{"up": int64(17), "down": int64(19)}).Error; err != nil {
		t.Fatalf("simulate traffic growth after caller snapshot: %v", err)
	}
	boundaryCalled := false
	svc := &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		boundaryCalled = true
		return func() {}, nil
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{item},
	)
	if err != nil {
		t.Fatalf("deadline-only repair after counter growth: %v", err)
	}
	if needRestart || result.NeedRestart || result.Updated != 1 || result.Items[0].TrafficReset {
		t.Fatalf("unexpected deadline-only result: %+v needRestart=%v", result, needRestart)
	}
	if boundaryCalled {
		t.Fatal("deadline-only repair unexpectedly advanced runtime baseline")
	}
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&traffic).Error; err != nil {
		t.Fatalf("reload traffic: %v", err)
	}
	if traffic.ExpiryTime != 200_000 || traffic.Up != 17 || traffic.Down != 19 || traffic.Enable {
		t.Fatalf("deadline-only repair did not preserve current counters: %+v", traffic)
	}
}

func TestRepairClientTrafficCyclesWithResetRejectsCounterGrowth(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	item := f.item(cycleRepairEmailA, 200_000, true)
	if err := database.GetDB().Model(&xray.ClientTraffic{}).
		Where("inbound_id = ? AND email = ?", f.inbound.Id, cycleRepairEmailA).
		UpdateColumns(map[string]any{"up": int64(17), "down": int64(19)}).Error; err != nil {
		t.Fatalf("simulate traffic growth after reset receipt: %v", err)
	}
	settingsBefore := mustCycleRepairInboundSettings(t, f.inbound.Id)
	var trafficBefore xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&trafficBefore).Error; err != nil {
		t.Fatalf("reload traffic before reset attempt: %v", err)
	}
	recordBefore := f.records[cycleRepairEmailA]
	boundaryCalled := false
	svc := &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		boundaryCalled = true
		return func() {}, nil
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{item},
	)
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "stale_traffic_precondition" {
		t.Fatalf("reset counter race error = %v", err)
	}
	if needRestart || result.Updated != 0 || result.Items[0].TrafficReset || boundaryCalled {
		t.Fatalf("stale reset changed state: result=%+v needRestart=%v boundary=%v", result, needRestart, boundaryCalled)
	}
	assertCycleRepairStateUnchanged(t, f.inbound.Id, settingsBefore, trafficBefore, recordBefore)
}

func TestRepairClientTrafficCyclesBoundaryFailureLeavesBatchUnchanged(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	settingsBefore := f.inbound.Settings
	trafficBefore := f.traffic[cycleRepairEmailA]
	recordBefore := f.records[cycleRepairEmailA]
	svc := &InboundService{trafficGenerationBoundary: func([]string) (func(), error) {
		return nil, errors.New("injected xray unavailable")
	}}

	result, needRestart, err := svc.RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{f.item(cycleRepairEmailA, 200_000, true)},
	)
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "runtime_boundary_failed" {
		t.Fatalf("error = %v", err)
	}
	if needRestart || result.Updated != 0 || result.NeedRestart {
		t.Fatalf("rejected result = %+v, needRestart=%v", result, needRestart)
	}
	assertCycleRepairStateUnchanged(t, f.inbound.Id, settingsBefore, trafficBefore, recordBefore)
}

func TestRepairClientTrafficCyclesRetryReceiptCannotResetNewTraffic(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	item := f.item(cycleRepairEmailA, 200_000, true)
	if _, _, err := cycleRepairService().RepairClientTrafficCycles(f.inbound.Id, []ClientTrafficCycleRepairItem{item}); err != nil {
		t.Fatalf("first repair: %v", err)
	}
	if err := database.GetDB().Model(&xray.ClientTraffic{}).
		Where("inbound_id = ? AND email = ?", f.inbound.Id, cycleRepairEmailA).
		UpdateColumns(map[string]any{"up": int64(17), "down": int64(19)}).Error; err != nil {
		t.Fatalf("simulate post-reset traffic: %v", err)
	}

	result, _, err := cycleRepairService().RepairClientTrafficCycles(f.inbound.Id, []ClientTrafficCycleRepairItem{item})
	if err == nil {
		t.Fatal("identical old receipt unexpectedly succeeded")
	}
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "stale_settings_precondition" {
		t.Fatalf("retry error = %v", err)
	}
	if result.Items[0].TrafficReset {
		t.Fatalf("rejected retry claims a reset: %+v", result.Items[0])
	}
	var row xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&row).Error; err != nil {
		t.Fatalf("reload traffic: %v", err)
	}
	if row.Up != 17 || row.Down != 19 {
		t.Fatalf("retry reset new traffic: up=%d down=%d", row.Up, row.Down)
	}
}

func TestRepairClientTrafficCyclesWaitsForCollectionGate(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	unlock := LockTrafficCollection()
	done := make(chan error, 1)
	go func() {
		_, _, err := cycleRepairService().RepairClientTrafficCycles(
			f.inbound.Id,
			[]ClientTrafficCycleRepairItem{f.item(cycleRepairEmailA, 200_000, true)},
		)
		done <- err
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("repair bypassed collection gate: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	var before xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", cycleRepairEmailA).First(&before).Error; err != nil {
		unlock()
		t.Fatalf("read while gated: %v", err)
	}
	if before.Up != 5 || before.Down != 7 {
		unlock()
		t.Fatalf("repair mutated before gate release: %+v", before)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repair after gate release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("repair did not complete after gate release")
	}
}

func TestRepairClientTrafficCyclesFailsClosedWithoutWriter(t *testing.T) {
	f := setupCycleRepairFixture(t, false)
	settingsBefore := f.inbound.Settings
	item := f.item(cycleRepairEmailA, 200_000, true)
	_, _, err := cycleRepairService().RepairClientTrafficCycles(f.inbound.Id, []ClientTrafficCycleRepairItem{item})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error = %v, want strict writer unavailable", err)
	}
	var inbound model.Inbound
	if err := database.GetDB().First(&inbound, f.inbound.Id).Error; err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	if inbound.Settings != settingsBefore {
		t.Fatal("strict writer failure mutated settings")
	}
}

func TestRepairClientTrafficCyclesRejectsMultiAttachment(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	record := f.records[cycleRepairEmailA]
	otherInbound := model.Inbound{
		Tag: "cycle-repair-other", Port: 42425, Protocol: model.VLESS, Enable: true,
		Settings: `{"clients":[]}`,
	}
	if err := database.GetDB().Create(&otherInbound).Error; err != nil {
		t.Fatalf("create other inbound: %v", err)
	}
	if err := database.GetDB().Create(&model.ClientInbound{ClientId: record.Id, InboundId: otherInbound.Id}).Error; err != nil {
		t.Fatalf("create second attachment: %v", err)
	}
	_, _, err := cycleRepairService().RepairClientTrafficCycles(
		f.inbound.Id,
		[]ClientTrafficCycleRepairItem{f.item(cycleRepairEmailA, 200_000, true)},
	)
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "client_link_identity_not_unique" {
		t.Fatalf("error = %v", err)
	}
}

func TestRepairClientTrafficCyclesResetRequiresCycleChange(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	item := f.item(cycleRepairEmailA, 200_000, true)
	item.Target = ClientTrafficCycleTarget{
		ExpiryTime: item.ExpectedSettings.ExpiryTime,
		Reset:      item.ExpectedSettings.Reset,
		Total:      item.ExpectedSettings.Total,
	}
	_, _, err := cycleRepairService().RepairClientTrafficCycles(f.inbound.Id, []ClientTrafficCycleRepairItem{item})
	var repairErr *ClientTrafficCycleRepairError
	if !errors.As(err, &repairErr) || repairErr.Code != "reset_requires_cycle_change" {
		t.Fatalf("error = %v", err)
	}
}

func TestRepairClientTrafficCyclesDatabaseSupportIsSQLiteOnly(t *testing.T) {
	if !clientTrafficCycleRepairDatabaseSupported(database.DialectSQLite) {
		t.Fatal("SQLite must be supported")
	}
	if clientTrafficCycleRepairDatabaseSupported(database.DialectPostgres) {
		t.Fatal("PostgreSQL must fail closed in v1")
	}
}

func TestUpdateInboundTakesInboundMutationLock(t *testing.T) {
	f := setupCycleRepairFixture(t, true)
	held := lockInbound(f.inbound.Id)
	done := make(chan error, 1)
	update := f.inbound
	update.Remark = "serialized admin update"
	go func() {
		_, _, err := (&InboundService{}).UpdateInbound(&update)
		done <- err
	}()
	select {
	case err := <-done:
		held.Unlock()
		t.Fatalf("UpdateInbound bypassed inbound mutation lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	held.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateInbound after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateInbound did not complete after lock release")
	}
}

func decodeCycleRepairTestJSON(t *testing.T, value string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return decoded
}

func mustCycleRepairInboundSettings(t *testing.T, inboundID int) string {
	t.Helper()
	var inbound model.Inbound
	if err := database.GetDB().First(&inbound, inboundID).Error; err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	return inbound.Settings
}

func assertCycleRepairStateUnchanged(
	t *testing.T,
	inboundID int,
	settings string,
	wantTraffic xray.ClientTraffic,
	wantRecord model.ClientRecord,
) {
	t.Helper()
	var inbound model.Inbound
	if err := database.GetDB().First(&inbound, inboundID).Error; err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	if inbound.Settings != settings {
		t.Fatalf("settings changed on rejected batch:\nbefore=%s\nafter=%s", settings, inbound.Settings)
	}
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", wantTraffic.Email).First(&traffic).Error; err != nil {
		t.Fatalf("reload traffic: %v", err)
	}
	if !reflect.DeepEqual(wantTraffic, traffic) {
		t.Fatalf("traffic changed on rejected batch: before=%+v after=%+v", wantTraffic, traffic)
	}
	var record model.ClientRecord
	if err := database.GetDB().Where("email = ?", wantRecord.Email).First(&record).Error; err != nil {
		t.Fatalf("reload record: %v", err)
	}
	if !reflect.DeepEqual(wantRecord, record) {
		t.Fatalf("record changed on rejected batch: before=%+v after=%+v", wantRecord, record)
	}
}
