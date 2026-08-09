package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/update"
)

func decodeData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, recorder.Body.String())
	}
	return envelope.Data
}

func TestAttachSingleEUICCIdentityFillsProfileGroupMetadataKey(t *testing.T) {
	groups := []map[string]any{{"eid": "", "aidHex": "", "profiles": []any{}}}
	chipInfo := map[string]any{
		"eids": []any{map[string]any{
			"eid": "89086030202200000025000015085962",
			"aid": "A0000005591010FFFFFFFF8900000100",
		}},
	}
	attachSingleEUICCIdentity(groups, chipInfo)
	if groups[0]["eid"] != "89086030202200000025000015085962" {
		t.Fatalf("group EID = %v", groups[0]["eid"])
	}
	if groups[0]["aidHex"] != "A0000005591010FFFFFFFF8900000100" {
		t.Fatalf("group AID = %v", groups[0]["aidHex"])
	}
}

func TestPhysicalMatchesConfigRejectsDuplicateAndroidSerialAlias(t *testing.T) {
	config := store.Device{
		ID:        "EC20",
		ATPort:    "/dev/serial/by-id/usb-Android_Android-if02-port0",
		USBPath:   "/sys/bus/usb/devices/1-6",
		ModemIMEI: "111111111111111",
	}
	newModem := device.Device{
		ID: "quectel-0125-1-5",
		Candidate: modem.Candidate{
			USBPath: "/sys/bus/usb/devices/1-5",
			ATPort: modem.Port{
				Path:       "/dev/ttyUSB6",
				StablePath: config.ATPort,
			},
		},
		Snapshot: &device.Snapshot{IMEI: "222222222222222"},
	}
	if physicalMatchesConfig(newModem, config) {
		t.Fatal("different modem matched through a duplicated Android by-id alias")
	}

	movedOriginal := newModem
	movedOriginal.Snapshot = &device.Snapshot{IMEI: config.ModemIMEI}
	if !physicalMatchesConfig(movedOriginal, config) {
		t.Fatal("same IMEI should follow the modem to a different USB port")
	}
}

func TestFindDiscoveredDevicePrefersPhysicalIdentityOverSerialAlias(t *testing.T) {
	alias := "/dev/serial/by-id/usb-Android_Android-if02-port0"
	devices := []device.Device{
		{ID: "old", Candidate: modem.Candidate{USBPath: "/sys/bus/usb/devices/1-6", ATPort: modem.Port{StablePath: alias}}},
		{ID: "new", Candidate: modem.Candidate{USBPath: "/sys/bus/usb/devices/1-5", ATPort: modem.Port{StablePath: alias}}},
	}
	selected := findDiscoveredDevice(devices, deviceConfigPayload{
		USBPath: "/sys/bus/usb/devices/1-5",
		ATPort:  alias,
	})
	if selected == nil || selected.ID != "new" {
		t.Fatalf("selected = %#v, want new physical USB device", selected)
	}
}

func TestHandleOperatorScanReturnsOperators(t *testing.T) {
	server := &Server{
		logger: regionTestLogger(),
		devices: fakeDeviceController{scanResult: device.OperatorScanResult{
			Status: "complete",
			Operators: []device.ScannedOperator{
				{Status: "current", Name: "China Mobile", Numeric: "46000", Act: "LTE"},
				{Status: "available", Name: "China Unicom", Numeric: "46001", Act: "LTE"},
			},
		}},
	}
	recorder := httptest.NewRecorder()
	server.handleOperatorScan(recorder, httptest.NewRequest(http.MethodGet, "/scan", nil), "dev1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	data := decodeData(t, recorder)
	if data["status"] != "complete" {
		t.Fatalf("scan status = %v", data["status"])
	}
	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("candidates = %v", data["candidates"])
	}
	first := candidates[0].(map[string]any)
	if first["plmn"] != "46000" || first["status"] != "current" || first["operatorName"] != "China Mobile" {
		t.Fatalf("first candidate = %v", first)
	}
}

func TestHandleOperatorScanStreamEmitsTerminalEvent(t *testing.T) {
	server := &Server{
		logger: regionTestLogger(),
		devices: fakeDeviceController{scanResult: device.OperatorScanResult{
			Status:    "complete",
			Operators: []device.ScannedOperator{{Status: "current", Name: "CMCC", Numeric: "46000"}},
		}},
	}
	recorder := httptest.NewRecorder()
	server.handleOperatorScanStream(recorder, httptest.NewRequest(http.MethodGet, "/scan/stream", nil), "dev1")
	body := recorder.Body.String()
	if !strings.Contains(body, "event: operator_scan") {
		t.Fatalf("expected operator_scan events, got %q", body)
	}
	if !strings.Contains(body, `"status":"running"`) || !strings.Contains(body, `"status":"complete"`) {
		t.Fatalf("expected running then complete, got %q", body)
	}
}

func TestHandleUSSDContinueAndCancel(t *testing.T) {
	server := &Server{
		logger:              regionTestLogger(),
		maxRequestBodyBytes: 4096,
		devices: fakeDeviceController{ussdResult: device.USSDResult{
			Status: "awaiting_input", Text: "Main menu", SessionID: "abc123", Continueable: true,
		}},
	}
	request := httptest.NewRequest(http.MethodPost, "/continue", strings.NewReader(`{"session_id":"abc123","input":"1"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleUSSDContinue(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("continue status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	data := decodeData(t, recorder)
	result, _ := data["result"].(map[string]any)
	if result["status"] != "awaiting_input" || data["session_id"] != "abc123" {
		t.Fatalf("continue data = %v", data)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/cancel", strings.NewReader(`{"session_id":"abc123"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleUSSDCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body=%s", cancelRec.Code, cancelRec.Body.String())
	}
}

func TestHandleUSSDContinueRequiresSession(t *testing.T) {
	server := &Server{logger: regionTestLogger(), maxRequestBodyBytes: 4096, devices: fakeDeviceController{}}
	request := httptest.NewRequest(http.MethodPost, "/continue", strings.NewReader(`{"input":"1"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleUSSDContinue(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing session status = %d, want 400", recorder.Code)
	}
}

func TestHandleCardPoliciesListsAll(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.UpsertCardPolicy(context.Background(), store.CardPolicy{
		ICCID: "89860001", NetworkEnabled: true, IPVersion: "IPV4V6", Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: database, logger: regionTestLogger()}
	recorder := httptest.NewRecorder()
	server.handleCardPolicies(recorder, httptest.NewRequest(http.MethodGet, "/api/cards/policies", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0]["iccid"] != "89860001" {
		t.Fatalf("policies = %v", envelope.Data)
	}
}

func TestHandleESIMShapes(t *testing.T) {
	server := &Server{logger: regionTestLogger()}

	overview := httptest.NewRecorder()
	server.handleESIM(overview, httptest.NewRequest(http.MethodGet, "/esim", nil), []string{}, "dev1", false)
	if overview.Code != http.StatusOK {
		t.Fatalf("overview status = %d", overview.Code)
	}
	if data := decodeData(t, overview); data["chipInfo"] != nil {
		t.Fatalf("overview chipInfo = %v, want nil (empty state)", data["chipInfo"])
	}

	profiles := httptest.NewRecorder()
	server.handleESIM(profiles, httptest.NewRequest(http.MethodGet, "/esim/profiles", nil), []string{"profiles"}, "dev1", false)
	if profiles.Code != http.StatusOK {
		t.Fatalf("profiles status = %d", profiles.Code)
	}

	notif := httptest.NewRecorder()
	server.handleESIM(notif, httptest.NewRequest(http.MethodGet, "/esim/notifications", nil), []string{"notifications"}, "dev1", false)
	if notif.Code != http.StatusOK {
		t.Fatalf("notifications status = %d", notif.Code)
	}

	// Download is a GET+SSE endpoint, so POST is rejected.
	downloadPost := httptest.NewRecorder()
	server.handleESIM(downloadPost, httptest.NewRequest(http.MethodPost, "/esim/actions/download", nil), []string{"actions", "download"}, "dev1", false)
	if downloadPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("download POST status = %d, want 405", downloadPost.Code)
	}

	// Download with no device manager reports 503.
	download := httptest.NewRecorder()
	server.handleESIM(download, httptest.NewRequest(http.MethodGet, "/esim/actions/download?smdp=rsp.example.com", nil), []string{"actions", "download"}, "dev1", false)
	if download.Code != http.StatusServiceUnavailable {
		t.Fatalf("download (no device) status = %d, want 503", download.Code)
	}

	// Switch with no physical modem present reports 503.
	absent := httptest.NewRecorder()
	server.handleESIM(absent, httptest.NewRequest(http.MethodPost, "/esim/actions/switch", strings.NewReader(`{"iccid":"8900000000000000001"}`)), []string{"actions", "switch"}, "dev1", false)
	if absent.Code != http.StatusServiceUnavailable {
		t.Fatalf("switch (no device) status = %d, want 503", absent.Code)
	}

	// Switch happy path: a present device + fake controller switches by ICCID.
	present := &Server{logger: regionTestLogger(), maxRequestBodyBytes: 4096, devices: fakeDeviceController{}}
	swOK := httptest.NewRecorder()
	swReq := httptest.NewRequest(http.MethodPost, "/esim/actions/switch", strings.NewReader(`{"iccid":"8900000000000000001","aid_hex":"A0"}`))
	swReq.Header.Set("Content-Type", "application/json")
	present.handleESIM(swOK, swReq, []string{"actions", "switch"}, "dev1", true)
	if swOK.Code != http.StatusOK {
		t.Fatalf("switch happy-path status = %d, body=%s", swOK.Code, swOK.Body.String())
	}
	if data := decodeData(t, swOK); data["status"] != "switched" || data["verified"] != true {
		t.Fatalf("switch data = %v", data)
	}

	// Disable happy path routes the active profile to ES10c DisableProfile.
	disableOK := httptest.NewRecorder()
	disableReq := httptest.NewRequest(http.MethodPost, "/esim/actions/disable", strings.NewReader(`{"iccid":"8900000000000000001","aid_hex":"A0000005591010FFFFFFFF8900000100"}`))
	disableReq.Header.Set("Content-Type", "application/json")
	present.handleESIM(disableOK, disableReq, []string{"actions", "disable"}, "dev1", true)
	if disableOK.Code != http.StatusOK {
		t.Fatalf("disable happy-path status = %d, body=%s", disableOK.Code, disableOK.Body.String())
	}
	if data := decodeData(t, disableOK); data["status"] != "disabled" || data["recovering"] != true {
		t.Fatalf("disable data = %v", data)
	}

	// Rename happy path routes PATCH to ES10c SetNickname support.
	renameOK := httptest.NewRecorder()
	renameReq := httptest.NewRequest(http.MethodPatch, "/esim/profiles/8900000000000000001", strings.NewReader(`{"name":"Test profile","aid_hex":"A0000005591010FFFFFFFF8900000100"}`))
	renameReq.Header.Set("Content-Type", "application/json")
	present.handleESIM(renameOK, renameReq, []string{"profiles", "8900000000000000001"}, "dev1", true)
	if renameOK.Code != http.StatusOK {
		t.Fatalf("rename happy-path status = %d, body=%s", renameOK.Code, renameOK.Body.String())
	}
	if data := decodeData(t, renameOK); data["status"] != "renamed" || data["name"] != "Test profile" {
		t.Fatalf("rename data = %v", data)
	}

	// Download on a present device but with no smdp address reports 400.
	dlNoSmdp := httptest.NewRecorder()
	present.handleESIM(dlNoSmdp, httptest.NewRequest(http.MethodGet, "/esim/actions/download", nil), []string{"actions", "download"}, "dev1", true)
	if dlNoSmdp.Code != http.StatusBadRequest {
		t.Fatalf("download (no smdp) status = %d, want 400", dlNoSmdp.Code)
	}
}

func TestHandleFixUSBNet(t *testing.T) {
	server := &Server{
		logger:              regionTestLogger(),
		maxRequestBodyBytes: 4096,
		devices:             fakeDeviceController{usbNetMode: device.USBNetMode{Mode: 0, Name: "QMI"}},
	}
	request := httptest.NewRequest(http.MethodPost, "/fix-usbnet", strings.NewReader(`{"at_port":"/dev/ttyUSB2","mode":0}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleFixUSBNet(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if data := decodeData(t, recorder); data["mode"] != float64(0) || data["name"] != "QMI" {
		t.Fatalf("fix-usbnet data = %v", data)
	}
}

func TestHandleUpdateApplyIsSafeNoop(t *testing.T) {
	server := &Server{logger: regionTestLogger()}
	recorder := httptest.NewRecorder()
	server.handleUpdateApply(recorder, httptest.NewRequest(http.MethodPost, "/apply", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if data := decodeData(t, recorder); data["applied"] != false {
		t.Fatalf("update apply must be a no-op, got %v", data)
	}
}

func TestHandleUpdateCheckUsesTrustedRepository(t *testing.T) {
	server := &Server{
		logger:           regionTestLogger(),
		updateRepository: update.DefaultRepository,
		updateCheck: func(_ context.Context, repo, token, current string) (update.CheckResult, error) {
			if repo != update.DefaultRepository || token != "token" || current == "" {
				t.Fatalf("check arguments = %q, %q, %q", repo, token, current)
			}
			return update.CheckResult{
				Available:    true,
				Current:      current,
				Latest:       "9.9.9",
				ReleaseNotes: "release notes",
			}, nil
		},
		updateToken: "token",
	}
	recorder := httptest.NewRecorder()
	server.handleUpdateCheck(recorder, httptest.NewRequest(http.MethodGet, "/check", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	data := decodeData(t, recorder)
	if data["available"] != true || data["version"] != "9.9.9" || data["repository"] != update.DefaultRepository {
		t.Fatalf("check data = %#v", data)
	}
}

func TestHandleUpdateApplyInstallsFromTrustedRepository(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.SetAdmin(context.Background(), "admin", []byte("hash")); err != nil {
		t.Fatal(err)
	}
	tokenHash := []byte("active-session")
	if err := database.CreateSession(
		context.Background(), 1, tokenHash, []byte("csrf"), time.Now().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		store:            database,
		logger:           regionTestLogger(),
		updateRepository: update.DefaultRepository,
		updateApply: func(_ context.Context, _ *slog.Logger, options update.Options, restart bool) (update.CheckResult, error) {
			if options.Repo != update.DefaultRepository || restart {
				t.Fatalf("apply options = %#v, restart = %v", options, restart)
			}
			return update.CheckResult{Applied: true, Latest: "9.9.9"}, nil
		},
	}
	recorder := httptest.NewRecorder()
	server.handleUpdateApply(recorder, httptest.NewRequest(http.MethodPost, "/apply", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	data := decodeData(t, recorder)
	if data["applied"] != true || data["version"] != "9.9.9" || data["reauthentication_required"] != true {
		t.Fatalf("apply data = %#v", data)
	}
	if _, err := database.SessionByTokenHash(context.Background(), tokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session must be revoked after update, got %v", err)
	}
	expired := map[string]bool{}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.MaxAge < 0 {
			expired[cookie.Name] = true
		}
	}
	if !expired[sessionCookieName] || !expired[csrfCookieName] {
		t.Fatalf("auth cookies were not expired: %#v", recorder.Result().Cookies())
	}
}

func TestE911WebsheetFlow(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	server := &Server{
		store:               database,
		logger:              regionTestLogger(),
		websheets:           newWebsheetManager(),
		maxRequestBodyBytes: 4096,
	}

	// 1. Create the websheet.
	createRec := httptest.NewRecorder()
	server.handleE911Websheet(createRec, httptest.NewRequest(http.MethodPost, "/e911", nil), store.Device{ID: "dev1"})
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", createRec.Code, createRec.Body.String())
	}
	createData := decodeData(t, createRec)
	embedURL, _ := createData["embed_url"].(string)
	if embedURL == "" || !strings.HasPrefix(embedURL, "/websheets/") {
		t.Fatalf("embed_url = %v", createData["embed_url"])
	}

	// 2. The form is served for a valid token.
	formRec := httptest.NewRecorder()
	server.handleWebsheet(formRec, httptest.NewRequest(http.MethodGet, embedURL, nil))
	if formRec.Code != http.StatusOK || !strings.Contains(formRec.Body.String(), "E911") {
		t.Fatalf("form status = %d", formRec.Code)
	}

	// 3. The callback stores the address, and done completes the session.
	callbackURL := strings.Replace(embedURL, "?", "/callback?", 1)
	callbackReq := httptest.NewRequest(http.MethodPost, callbackURL, strings.NewReader(`{"street":"1 Main St","city":"Springfield","country":"US"}`))
	callbackReq.Header.Set("Content-Type", "application/json")
	callbackRec := httptest.NewRecorder()
	server.handleWebsheet(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body=%s", callbackRec.Code, callbackRec.Body.String())
	}
	stored, err := database.AppSetting(context.Background(), "e911_address:dev1")
	if err != nil || !strings.Contains(string(stored.Value), "Springfield") {
		t.Fatalf("e911 address not persisted: %v %v", stored, err)
	}

	doneURL := strings.Replace(embedURL, "?", "/done?", 1)
	doneRec := httptest.NewRecorder()
	server.handleWebsheet(doneRec, httptest.NewRequest(http.MethodPost, doneURL, nil))
	if doneRec.Code != http.StatusOK {
		t.Fatalf("done status = %d", doneRec.Code)
	}
}

func TestE911WebsheetRejectsBadToken(t *testing.T) {
	server := &Server{logger: regionTestLogger(), websheets: newWebsheetManager()}
	session := server.websheets.create("dev1")
	recorder := httptest.NewRecorder()
	server.handleWebsheet(recorder, httptest.NewRequest(http.MethodGet, "/websheets/"+session.id+"?token=wrong", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("bad token status = %d, want 403", recorder.Code)
	}
}
