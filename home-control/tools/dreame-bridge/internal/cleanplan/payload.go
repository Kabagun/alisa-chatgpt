package cleanplan

import "encoding/json"

// LogicalPayloadJSON serializes only the fixed START_CUSTOM logical RPC for an
// already validated draft. It is for offline review/tests, not dispatch. It has
// no endpoint, signature, credentials, authorization token, or transport.
//
// Re-checking the injected clock prevents serializing an expired draft. Holding
// these bytes does not authorize later use; a future writer must rebuild fresh
// observations and compare them to the concrete approved plan.
func (d *Draft) LogicalPayloadJSON(nowMillis int64) ([]byte, error) {
	if !d.valid() {
		return nil, fail(CodeDraftInvalid)
	}
	if nowMillis < d.builtAt || nowMillis-d.oldestRead > MaxAgeMillis {
		return nil, fail(CodeSnapshotStale)
	}
	selection := struct {
		Selects [1][5]int `json:"selects"`
	}{Selects: [1][5]int{{d.segmentID, 1, d.fan, d.water, 1}}}
	parameters, err := json.Marshal(selection)
	if err != nil {
		return nil, fail(CodeDraftInvalid)
	}
	type input struct {
		PIID  int `json:"piid"`
		Value any `json:"value"`
	}
	payload := struct {
		Method string `json:"method"`
		Params struct {
			DID  string   `json:"did"`
			SIID int      `json:"siid"`
			AIID int      `json:"aiid"`
			In   [2]input `json:"in"`
		} `json:"params"`
	}{Method: "action"}
	payload.Params.DID = d.deviceID
	payload.Params.SIID = 4
	payload.Params.AIID = 1
	payload.Params.In = [2]input{{PIID: 1, Value: 18}, {PIID: 10, Value: string(parameters)}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fail(CodeDraftInvalid)
	}
	return encoded, nil
}
