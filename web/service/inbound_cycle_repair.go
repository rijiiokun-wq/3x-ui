package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mhsanaei/3x-ui/v3/database"
	"github.com/mhsanaei/3x-ui/v3/database/model"
	"github.com/mhsanaei/3x-ui/v3/xray"

	"gorm.io/gorm"
)

const maxClientTrafficCycleRepairItems = 50

// ClientTrafficCycleSettingsExpected is the exact per-inbound client state the
// caller observed in settings.clients. It is intentionally independent from
// ClientTraffic: legacy databases can contain safely reconcilable drift.
type ClientTrafficCycleSettingsExpected struct {
	ExpiryTime int64 `json:"expiryTime"`
	Reset      int   `json:"reset"`
	Total      int64 `json:"total"`
	Enable     bool  `json:"enable"`
}

// ClientTrafficCycleTrafficExpected is the ClientTraffic state observed by the
// caller. Policy fields are always exact. Up/Down are an exact CAS boundary for
// resets; deadline-only repairs allow monotonic counter growth and preserve the
// newer values instead of rejecting active clients.
type ClientTrafficCycleTrafficExpected struct {
	ExpiryTime int64 `json:"expiryTime"`
	Reset      int   `json:"reset"`
	Total      int64 `json:"total"`
	Enable     bool  `json:"enable"`
	Up         int64 `json:"up"`
	Down       int64 `json:"down"`
}

// ClientTrafficCycleTarget contains the only limit fields this maintenance
// endpoint is allowed to change.  Enable is deliberately not caller supplied:
// ResetTraffic is the sole operation which can set it, and only to true.
type ClientTrafficCycleTarget struct {
	ExpiryTime int64 `json:"expiryTime"`
	Reset      int   `json:"reset"`
	Total      int64 `json:"total"`
}

// ClientTrafficCycleRepairItem identifies one client in one inbound and gives
// compare-and-swap preconditions for the settings and normalized SQL rows.
type ClientTrafficCycleRepairItem struct {
	InboundID        int                                `json:"inboundId"`
	Email            string                             `json:"email"`
	UUID             string                             `json:"uuid"`
	SubID            string                             `json:"subId"`
	ExpectedSettings ClientTrafficCycleSettingsExpected `json:"expectedSettings"`
	ExpectedTraffic  ClientTrafficCycleTrafficExpected  `json:"expectedTraffic"`
	Target           ClientTrafficCycleTarget           `json:"target"`
	ResetTraffic     bool                               `json:"resetTraffic"`
}

type ClientTrafficCycleRepairItemResult struct {
	Index        int    `json:"index"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	TrafficReset bool   `json:"trafficReset"`
	Reenabled    bool   `json:"reenabled"`
}

type ClientTrafficCycleRepairResult struct {
	Updated     int                                  `json:"updated"`
	NeedRestart bool                                 `json:"needRestart"`
	Items       []ClientTrafficCycleRepairItemResult `json:"items"`
}

// ClientTrafficCycleRepairError intentionally contains no client identity.
// Controller responses and logs can safely expose the item index and stable
// machine reason while the sensitive request stays out of the error string.
type ClientTrafficCycleRepairError struct {
	Index int
	Code  string
	cause error
}

func (e *ClientTrafficCycleRepairError) Error() string {
	if e.Index >= 0 {
		return fmt.Sprintf("client traffic cycle repair item %d rejected: %s", e.Index, e.Code)
	}
	return "client traffic cycle repair batch rejected: " + e.Code
}

func (e *ClientTrafficCycleRepairError) Unwrap() error { return e.cause }

type clientTrafficCycleRepairPlan struct {
	item          ClientTrafficCycleRepairItem
	settingsIndex int
	traffic       xray.ClientTraffic
	record        model.ClientRecord
	reenabled     bool
}

// RepairClientTrafficCycles atomically applies a compare-and-swap batch to one
// inbound.  The complete read/validate/write sequence is serialized with every
// other traffic mutation, then protected by the same per-inbound mutex used by
// client edits.  One stale or invalid item rolls the whole SQL transaction back.
func (s *InboundService) RepairClientTrafficCycles(
	inboundID int,
	items []ClientTrafficCycleRepairItem,
) (result ClientTrafficCycleRepairResult, needRestart bool, err error) {
	result.Items = make([]ClientTrafficCycleRepairItemResult, len(items))
	for i := range result.Items {
		result.Items[i] = ClientTrafficCycleRepairItemResult{
			Index:  i,
			Status: "not_applied",
			Reason: "batch_rejected",
		}
	}

	// Deadlock-safe global order: collection gate -> strict writer -> inbound
	// lock -> transaction.  XrayTrafficJob uses the same first two layers.
	unlockCollection := LockTrafficCollection()
	defer unlockCollection()

	err = submitTrafficWriteStrict(func() error {
		defer lockInbound(inboundID).Unlock()
		var boundaryRestore func()
		boundaryCommitted := false
		defer func() {
			if !boundaryCommitted && boundaryRestore != nil {
				boundaryRestore()
			}
		}()

		innerErr := database.GetDB().Transaction(func(tx *gorm.DB) error {
			return s.repairClientTrafficCyclesTx(tx, inboundID, items, &result, &boundaryRestore)
		})
		if innerErr != nil {
			result.Updated = 0
			result.NeedRestart = false
			for i := range result.Items {
				if result.Items[i].Status == "updated" {
					result.Items[i].Status = "not_applied"
					result.Items[i].Reason = "batch_rolled_back"
					result.Items[i].TrafficReset = false
					result.Items[i].Reenabled = false
				}
			}
			return innerErr
		}
		boundaryCommitted = true
		needRestart = result.NeedRestart
		return nil
	})
	if err != nil {
		needRestart = false
	}
	return result, needRestart, err
}

func (s *InboundService) repairClientTrafficCyclesTx(
	tx *gorm.DB,
	inboundID int,
	items []ClientTrafficCycleRepairItem,
	result *ClientTrafficCycleRepairResult,
	boundaryRestore *func(),
) error {
	if inboundID <= 0 {
		return rejectClientTrafficCycleRepair(result, -1, "invalid_inbound_id", nil)
	}
	if len(items) == 0 || len(items) > maxClientTrafficCycleRepairItems {
		return rejectClientTrafficCycleRepair(result, -1, "invalid_batch_size", nil)
	}
	if !clientTrafficCycleRepairDatabaseSupported(tx.Dialector.Name()) {
		return rejectClientTrafficCycleRepair(result, -1, "database_unsupported", nil)
	}

	var inbound model.Inbound
	if err := tx.Where("id = ?", inboundID).First(&inbound).Error; err != nil {
		code := "inbound_read_failed"
		if errors.Is(err, gorm.ErrRecordNotFound) {
			code = "inbound_not_found"
		}
		return rejectClientTrafficCycleRepair(result, -1, code, err)
	}
	if inbound.NodeID != nil {
		return rejectClientTrafficCycleRepair(result, -1, "remote_inbound_unsupported", nil)
	}
	if inbound.Protocol != model.VLESS {
		return rejectClientTrafficCycleRepair(result, -1, "protocol_unsupported", nil)
	}

	originalSettings := inbound.Settings
	root, clients, err := decodeInboundCycleRepairSettings(originalSettings)
	if err != nil {
		return rejectClientTrafficCycleRepair(result, -1, "invalid_inbound_settings", err)
	}

	seenEmails := make(map[string]struct{}, len(items))
	seenUUIDs := make(map[string]struct{}, len(items))
	plans := make([]clientTrafficCycleRepairPlan, 0, len(items))
	for index, item := range items {
		if code := validateClientTrafficCycleRepairItem(inboundID, item); code != "" {
			return rejectClientTrafficCycleRepair(result, index, code, nil)
		}
		if _, exists := seenEmails[item.Email]; exists {
			return rejectClientTrafficCycleRepair(result, index, "duplicate_email", nil)
		}
		if _, exists := seenUUIDs[item.UUID]; exists {
			return rejectClientTrafficCycleRepair(result, index, "duplicate_uuid", nil)
		}
		seenEmails[item.Email] = struct{}{}
		seenUUIDs[item.UUID] = struct{}{}

		settingsIndex, code := exactCycleRepairSettingsClient(clients, item)
		if code != "" {
			return rejectClientTrafficCycleRepair(result, index, code, nil)
		}

		var trafficRows []xray.ClientTraffic
		if err := tx.Where("inbound_id = ? AND email = ?", inboundID, item.Email).Limit(2).Find(&trafficRows).Error; err != nil {
			return rejectClientTrafficCycleRepair(result, index, "traffic_read_failed", err)
		}
		if len(trafficRows) != 1 {
			return rejectClientTrafficCycleRepair(result, index, "traffic_identity_not_unique", nil)
		}
		traffic := trafficRows[0]
		if traffic.InboundId != inboundID {
			return rejectClientTrafficCycleRepair(result, index, "traffic_inbound_mismatch", nil)
		}
		if !cycleRepairTrafficMatchesExpected(traffic, item.ExpectedTraffic, item.ResetTraffic) {
			return rejectClientTrafficCycleRepair(result, index, "stale_traffic_precondition", nil)
		}

		var records []model.ClientRecord
		if err := tx.Where("email = ?", item.Email).Limit(2).Find(&records).Error; err != nil {
			return rejectClientTrafficCycleRepair(result, index, "client_record_read_failed", err)
		}
		if len(records) != 1 {
			return rejectClientTrafficCycleRepair(result, index, "client_record_identity_not_unique", nil)
		}
		record := records[0]
		if record.Email != item.Email || record.UUID != item.UUID || record.SubID != item.SubID {
			return rejectClientTrafficCycleRepair(result, index, "client_record_identity_mismatch", nil)
		}
		var links []model.ClientInbound
		if err := tx.Where("client_id = ?", record.Id).Limit(2).Find(&links).Error; err != nil {
			return rejectClientTrafficCycleRepair(result, index, "client_link_read_failed", err)
		}
		if len(links) != 1 || links[0].InboundId != inboundID {
			return rejectClientTrafficCycleRepair(result, index, "client_link_identity_not_unique", nil)
		}

		plans = append(plans, clientTrafficCycleRepairPlan{
			item:          item,
			settingsIndex: settingsIndex,
			traffic:       traffic,
			record:        record,
			reenabled: item.ResetTraffic && (!item.ExpectedSettings.Enable ||
				!item.ExpectedTraffic.Enable ||
				!record.Enable),
		})
	}

	resetEmails := make([]string, 0, len(plans))
	for _, plan := range plans {
		if plan.item.ResetTraffic {
			resetEmails = append(resetEmails, plan.item.Email)
		}
	}
	if len(resetEmails) > 0 {
		restore, err := s.beginClientTrafficGenerationBoundary(resetEmails)
		if err != nil || restore == nil {
			return rejectClientTrafficCycleRepair(result, -1, "runtime_boundary_failed", err)
		}
		*boundaryRestore = restore
	}

	// All reads and preconditions above complete before the first write.  The
	// conditional updates below repeat those preconditions at write time so a
	// direct/out-of-process DB writer also causes a full rollback.
	for index, plan := range plans {
		updatedClient, err := applyCycleRepairSettingsTarget(clients[plan.settingsIndex], plan.item)
		if err != nil {
			return rejectClientTrafficCycleRepair(result, index, "settings_update_failed", err)
		}
		clients[plan.settingsIndex] = updatedClient

		trafficUpdates := map[string]any{
			"expiry_time": plan.item.Target.ExpiryTime,
			"reset":       plan.item.Target.Reset,
			"total":       plan.item.Target.Total,
		}
		if plan.item.ResetTraffic {
			trafficUpdates["up"] = int64(0)
			trafficUpdates["down"] = int64(0)
			trafficUpdates["enable"] = true
		}
		trafficWrite := tx.Model(&xray.ClientTraffic{}).
			Where(
				"id = ? AND inbound_id = ? AND email = ? AND expiry_time = ? AND reset = ? AND total = ? AND enable = ?",
				plan.traffic.Id,
				inboundID,
				plan.item.Email,
				plan.item.ExpectedTraffic.ExpiryTime,
				plan.item.ExpectedTraffic.Reset,
				plan.item.ExpectedTraffic.Total,
				plan.item.ExpectedTraffic.Enable,
			)
		if plan.item.ResetTraffic {
			trafficWrite = trafficWrite.Where(
				"up = ? AND down = ?",
				plan.item.ExpectedTraffic.Up,
				plan.item.ExpectedTraffic.Down,
			)
		} else {
			// Xray collection may legitimately advance counters between the
			// caller's direct read and this transaction. A deadline-only repair
			// never writes counters, so accept monotonic growth but reject a
			// decrease that may indicate an intervening reset.
			trafficWrite = trafficWrite.Where(
				"up >= ? AND down >= ?",
				plan.item.ExpectedTraffic.Up,
				plan.item.ExpectedTraffic.Down,
			)
		}
		trafficWrite = trafficWrite.Updates(trafficUpdates)
		if trafficWrite.Error != nil {
			return rejectClientTrafficCycleRepair(result, index, "traffic_write_failed", trafficWrite.Error)
		}
		if trafficWrite.RowsAffected != 1 {
			return rejectClientTrafficCycleRepair(result, index, "stale_traffic_write", nil)
		}

		recordUpdates := map[string]any{
			"expiry_time": plan.item.Target.ExpiryTime,
			"reset":       plan.item.Target.Reset,
			"total_gb":    plan.item.Target.Total,
		}
		if plan.item.ResetTraffic {
			recordUpdates["enable"] = true
		}
		recordWrite := tx.Model(&model.ClientRecord{}).
			Where(
				"id = ? AND email = ? AND uuid = ? AND sub_id = ? AND expiry_time = ? AND reset = ? AND total_gb = ? AND enable = ?",
				plan.record.Id,
				plan.item.Email,
				plan.item.UUID,
				plan.item.SubID,
				plan.record.ExpiryTime,
				plan.record.Reset,
				plan.record.TotalGB,
				plan.record.Enable,
			).
			UpdateColumns(recordUpdates)
		if recordWrite.Error != nil {
			return rejectClientTrafficCycleRepair(result, index, "client_record_write_failed", recordWrite.Error)
		}
		if recordWrite.RowsAffected != 1 {
			return rejectClientTrafficCycleRepair(result, index, "stale_client_record_write", nil)
		}

		result.Items[index] = ClientTrafficCycleRepairItemResult{
			Index:        index,
			Status:       "updated",
			Reason:       "applied",
			TrafficReset: plan.item.ResetTraffic,
			Reenabled:    plan.reenabled,
		}
	}

	encodedClients, err := json.Marshal(clients)
	if err != nil {
		return rejectClientTrafficCycleRepair(result, -1, "settings_encode_failed", err)
	}
	root["clients"] = encodedClients
	newSettings, err := json.Marshal(root)
	if err != nil {
		return rejectClientTrafficCycleRepair(result, -1, "settings_encode_failed", err)
	}
	inboundWrite := tx.Model(&model.Inbound{}).
		Where("id = ? AND settings = ?", inboundID, originalSettings).
		Update("settings", string(newSettings))
	if inboundWrite.Error != nil {
		return rejectClientTrafficCycleRepair(result, -1, "inbound_write_failed", inboundWrite.Error)
	}
	if inboundWrite.RowsAffected != 1 {
		return rejectClientTrafficCycleRepair(result, -1, "stale_inbound_settings", nil)
	}

	result.Updated = len(plans)
	for _, plan := range plans {
		if plan.reenabled {
			result.NeedRestart = true
			break
		}
	}
	return nil
}

func (s *InboundService) beginClientTrafficGenerationBoundary(emails []string) (func(), error) {
	if s.trafficGenerationBoundary != nil {
		return s.trafficGenerationBoundary(emails)
	}
	return (&XrayService{}).BeginClientTrafficGenerationBoundary(emails)
}

func clientTrafficCycleRepairDatabaseSupported(dialect string) bool {
	return dialect == database.DialectSQLite
}

func validateClientTrafficCycleRepairItem(inboundID int, item ClientTrafficCycleRepairItem) string {
	if item.InboundID != inboundID {
		return "inbound_id_mismatch"
	}
	if item.Email == "" || strings.TrimSpace(item.Email) != item.Email {
		return "invalid_email"
	}
	if _, err := uuid.Parse(item.UUID); err != nil {
		return "invalid_uuid"
	}
	if item.SubID == "" || strings.TrimSpace(item.SubID) != item.SubID {
		return "invalid_sub_id"
	}
	if item.ExpectedSettings.Reset < 0 || item.ExpectedSettings.Total < 0 ||
		item.ExpectedTraffic.Reset < 0 || item.ExpectedTraffic.Total < 0 ||
		item.ExpectedTraffic.Up < 0 || item.ExpectedTraffic.Down < 0 {
		return "invalid_expected_state"
	}
	if item.Target.ExpiryTime <= 0 || item.Target.Reset < 0 || item.Target.Total <= 0 {
		return "invalid_target"
	}
	if item.ResetTraffic &&
		item.Target.ExpiryTime == item.ExpectedSettings.ExpiryTime &&
		item.Target.Reset == item.ExpectedSettings.Reset &&
		item.Target.Total == item.ExpectedSettings.Total {
		return "reset_requires_cycle_change"
	}
	return ""
}

func rejectClientTrafficCycleRepair(
	result *ClientTrafficCycleRepairResult,
	index int,
	code string,
	cause error,
) error {
	if index >= 0 && index < len(result.Items) {
		result.Items[index].Status = "rejected"
		result.Items[index].Reason = code
	}
	return &ClientTrafficCycleRepairError{Index: index, Code: code, cause: cause}
}

func decodeInboundCycleRepairSettings(settings string) (map[string]json.RawMessage, []json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &root); err != nil {
		return nil, nil, err
	}
	rawClients, ok := root["clients"]
	if !ok {
		return nil, nil, errors.New("clients field is missing")
	}
	var clients []json.RawMessage
	if err := json.Unmarshal(rawClients, &clients); err != nil {
		return nil, nil, err
	}
	return root, clients, nil
}

func exactCycleRepairSettingsClient(
	clients []json.RawMessage,
	item ClientTrafficCycleRepairItem,
) (int, string) {
	candidateIndex := -1
	for index, rawClient := range clients {
		var client map[string]json.RawMessage
		if err := json.Unmarshal(rawClient, &client); err != nil {
			return -1, "invalid_settings_client"
		}
		email, emailOK := rawString(client, "email")
		clientUUID, uuidOK := rawString(client, "id")
		if (emailOK && email == item.Email) || (uuidOK && clientUUID == item.UUID) {
			if candidateIndex >= 0 {
				return -1, "settings_identity_not_unique"
			}
			candidateIndex = index
		}
	}
	if candidateIndex < 0 {
		return -1, "settings_client_not_found"
	}

	var client map[string]json.RawMessage
	if err := json.Unmarshal(clients[candidateIndex], &client); err != nil {
		return -1, "invalid_settings_client"
	}
	email, emailOK := rawString(client, "email")
	clientUUID, uuidOK := rawString(client, "id")
	subID, subIDOK := rawString(client, "subId")
	if !emailOK || !uuidOK || !subIDOK || email != item.Email || clientUUID != item.UUID || subID != item.SubID {
		return -1, "settings_identity_mismatch"
	}
	expiryTime, expiryOK := rawInt64(client, "expiryTime")
	reset, resetOK := rawInt(client, "reset")
	total, totalOK := rawInt64(client, "totalGB")
	enable, enableOK := rawBool(client, "enable")
	if !expiryOK || !resetOK || !totalOK || !enableOK {
		return -1, "settings_state_incomplete"
	}
	if expiryTime != item.ExpectedSettings.ExpiryTime ||
		reset != item.ExpectedSettings.Reset ||
		total != item.ExpectedSettings.Total ||
		enable != item.ExpectedSettings.Enable {
		return -1, "stale_settings_precondition"
	}
	return candidateIndex, ""
}

func applyCycleRepairSettingsTarget(rawClient json.RawMessage, item ClientTrafficCycleRepairItem) (json.RawMessage, error) {
	var client map[string]json.RawMessage
	if err := json.Unmarshal(rawClient, &client); err != nil {
		return nil, err
	}
	client["expiryTime"] = rawJSON(item.Target.ExpiryTime)
	client["reset"] = rawJSON(item.Target.Reset)
	client["totalGB"] = rawJSON(item.Target.Total)
	if item.ResetTraffic {
		client["enable"] = rawJSON(true)
	}
	return json.Marshal(client)
}

func cycleRepairTrafficMatchesExpected(
	row xray.ClientTraffic,
	expected ClientTrafficCycleTrafficExpected,
	resetTraffic bool,
) bool {
	policyMatches := row.ExpiryTime == expected.ExpiryTime &&
		row.Reset == expected.Reset &&
		row.Total == expected.Total &&
		row.Enable == expected.Enable
	if !policyMatches {
		return false
	}
	if resetTraffic {
		return row.Up == expected.Up && row.Down == expected.Down
	}
	return row.Up >= expected.Up && row.Down >= expected.Down
}

func rawJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func rawString(values map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := values[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func rawInt64(values map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func rawInt(values map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func rawBool(values map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := values[key]
	if !ok {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}
