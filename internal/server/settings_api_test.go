package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vocat/internal/store"
)

type settingsAPITest struct {
	server   *Server
	database *store.Store
}

func newSettingsAPITest(t *testing.T) settingsAPITest {
	t.Helper()
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return settingsAPITest{
		server: &Server{
			store:               database,
			logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
			maxRequestBodyBytes: 1 << 20,
		},
		database: database,
	}
}

func (test settingsAPITest) request(
	t *testing.T,
	method string,
	target string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	cleanPath := strings.Trim(strings.TrimPrefix(request.URL.Path, "/api"), "/")
	if !test.server.routeSettingsAPI(recorder, request, cleanPath) {
		writeError(recorder, http.StatusNotFound, "not_found", "API endpoint not found")
	}
	return recorder
}

func decodeSettingsResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return response
}

func TestNotificationSettingsAlwaysReturnsFiveChannelsAndPreservesSecrets(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(t, http.MethodGet, "/api/settings/notifications", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", recorder.Code, recorder.Body)
	}
	response := decodeSettingsResponse(t, recorder)
	data, ok := response["data"].(map[string]any)
	if !ok || len(data) != len(notificationChannels) {
		t.Fatalf("notification channels = %#v", response["data"])
	}
	for _, channel := range notificationChannels {
		config, ok := data[channel].(map[string]any)
		if !ok || config["enabled"] != false {
			t.Fatalf("missing disabled channel %q: %#v", channel, config)
		}
	}

	if err := test.database.UpsertNotificationSetting(
		context.Background(),
		store.NotificationSetting{
			Channel: "telegram",
			Enabled: true,
			Config: json.RawMessage(
				`{"bot_token":"123456:abcdefghijklmnopqrstuvwxyz","chat_id":"1"}`,
			),
		},
	); err != nil {
		t.Fatal(err)
	}
	recorder = test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"telegram":{"enabled":true,"bot_token":"********","chat_id":"2"}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("abcdefghijklmnopqrstuvwxyz")) {
		t.Fatalf("PUT response leaked secret: %s", recorder.Body)
	}
	response = decodeSettingsResponse(t, recorder)
	data = response["data"].(map[string]any)
	telegram := data["telegram"].(map[string]any)
	if telegram["bot_token"] != store.SecretMask || telegram["chat_id"] != "2" {
		t.Fatalf("redacted Telegram config = %#v", telegram)
	}
	stored, err := test.database.NotificationSetting(context.Background(), "telegram")
	if err != nil {
		t.Fatal(err)
	}
	var storedConfig map[string]any
	if err := json.Unmarshal(stored.Config, &storedConfig); err != nil {
		t.Fatal(err)
	}
	if storedConfig["bot_token"] != "123456:abcdefghijklmnopqrstuvwxyz" ||
		storedConfig["chat_id"] != "2" {
		t.Fatalf("stored Telegram config = %#v", storedConfig)
	}
}

func TestNotificationSettingsRejectsUnknownAndMalformedInput(t *testing.T) {
	test := newSettingsAPITest(t)
	cases := []struct {
		name string
		body string
		code string
	}{
		{
			name: "unknown channel",
			body: `{"pagerduty":{"enabled":true}}`,
			code: "invalid_notification_channel",
		},
		{
			name: "missing enabled",
			body: `{"telegram":{"chat_id":"1"}}`,
			code: "invalid_notification_config",
		},
		{
			name: "wrong field type",
			body: `{"webhook":{"enabled":true,"urls":"https://example.com"}}`,
			code: "invalid_notification_config",
		},
		{
			name: "invalid Telegram chat id",
			body: `{"telegram":{"enabled":true,"chat_id":"group-name"}}`,
			code: "invalid_notification_config",
		},
		{
			name: "invalid Telegram admin id",
			body: `{"telegram":{"enabled":true,"admin_id":"-1"}}`,
			code: "invalid_notification_config",
		},
		{
			name: "insecure Telegram base URL",
			body: `{"telegram":{"enabled":true,"base_url":"http://example.com"}}`,
			code: "invalid_notification_config",
		},
		{
			name: "unknown field",
			body: `{"email":{"enabled":false,"smtp_host":"mail.example.com","typo":1}}`,
			code: "invalid_notification_config",
		},
		{
			name: "header value with newline",
			body: `{"webhook":{"enabled":true,"headers":{"X-Api-Key":"a\nb"}}}`,
			code: "invalid_notification_config",
		},
		{
			name: "header name with colon",
			body: `{"webhook":{"enabled":true,"headers":{"X:Bad":"v"}}}`,
			code: "invalid_notification_config",
		},
		{
			name: "null body",
			body: `null`,
			code: "invalid_request",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := test.request(
				t,
				http.MethodPut,
				"/api/settings/notifications",
				item.body,
			)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
			response := decodeSettingsResponse(t, recorder)
			detail := response["error"].(map[string]any)
			if detail["code"] != item.code {
				t.Fatalf("error = %#v", detail)
			}
		})
	}
}

func TestNotificationSettingsAcceptsTelegramReverseProxyTemplate(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"telegram":{"enabled":true,"bot_token":"123456:abcdefghijklmnopqrstuvwxyz","chat_id":"1","base_url":"https://telegram.example.com/bot%s/%s"}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	stored, err := test.database.NotificationSetting(context.Background(), "telegram")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(stored.Config, &config); err != nil {
		t.Fatal(err)
	}
	if config["base_url"] != "https://telegram.example.com/bot%s/%s" {
		t.Fatalf("stored Telegram base URL = %#v", config["base_url"])
	}
}

func TestNotificationTestsBlockSSRFAndUnsupportedChannels(t *testing.T) {
	test := newSettingsAPITest(t)
	var webhookHits atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()

	recorder := test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/webhook/test",
		`{"urls":[`+strconvJSON(local.URL)+`]}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("webhook SSRF status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if webhookHits.Load() != 0 {
		t.Fatalf("blocked webhook reached local service %d times", webhookHits.Load())
	}
	response := decodeSettingsResponse(t, recorder)
	if response["error"].(map[string]any)["code"] != "unsafe_destination" {
		t.Fatalf("webhook SSRF response = %#v", response)
	}

	recorder = test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/telegram/test",
		`{"bot_token":"123456:abcdefghijklmnopqrstuvwxyz","chat_id":"1","base_url":"https://169.254.169.254"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("Telegram metadata status = %d, body = %s", recorder.Code, recorder.Body)
	}

	recorder = test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/email/test",
		`{"smtp_host":"127.0.0.1","smtp_port":25,"from_address":"from@example.com","to_addresses":["to@example.com"]}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("SMTP SSRF status = %d, body = %s", recorder.Code, recorder.Body)
	}

	recorder = test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/bark/test",
		`{"urls":[`+strconvJSON(local.URL)+`]}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bark SSRF status = %d, body = %s", recorder.Code, recorder.Body)
	}
	response = decodeSettingsResponse(t, recorder)
	if response["error"].(map[string]any)["code"] != "unsafe_destination" {
		t.Fatalf("bark SSRF response = %#v", response)
	}

	recorder = test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/bark/test",
		`{}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bark empty status = %d", recorder.Code)
	}
	response = decodeSettingsResponse(t, recorder)
	if response["error"].(map[string]any)["code"] != "notification_not_configured" {
		t.Fatalf("bark empty response = %#v", response)
	}

	// pushplus is a supported channel but has no connectivity test.
	recorder = test.request(
		t,
		http.MethodPost,
		"/api/settings/notifications/pushplus/test",
		`{}`,
	)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported notification status = %d", recorder.Code)
	}
	response = decodeSettingsResponse(t, recorder)
	if response["error"].(map[string]any)["code"] != "notification_test_unsupported" {
		t.Fatalf("unsupported response = %#v", response)
	}

	// Removed channels (feishu, qq, weixin) are no longer recognised at all.
	for _, removed := range []string{"feishu", "qq", "weixin"} {
		recorder = test.request(
			t,
			http.MethodPost,
			"/api/settings/notifications/"+removed+"/test",
			`{}`,
		)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("removed channel %q status = %d", removed, recorder.Code)
		}
	}
}

func strconvJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestNotificationWebhookHeadersRoundTrip(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"webhook":{"enabled":true,"urls":["https://example.com/hook"],`+
			`"timeout_ms":30000,"retry_max":2,"headers":{"X-Api-Key":"abc"}}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	stored, err := test.database.NotificationSetting(context.Background(), "webhook")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(stored.Config, &config); err != nil {
		t.Fatal(err)
	}
	headers, ok := config["headers"].(map[string]any)
	if !ok || headers["X-Api-Key"] != "abc" {
		t.Fatalf("stored webhook headers = %#v", config)
	}
	if config["timeout_ms"] != float64(30000) {
		t.Fatalf("stored webhook timeout = %#v", config["timeout_ms"])
	}
}

func TestNotificationEmailUseSslRoundTrip(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"email":{"enabled":true,"use_ssl":true,"smtp_host":"smtp.example.com","smtp_port":465,`+
			`"username":"u@example.com","password":"mail_secret","from_address":"u@example.com",`+
			`"to_addresses":["a@example.com"]}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	stored, err := test.database.NotificationSetting(context.Background(), "email")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(stored.Config, &config); err != nil {
		t.Fatal(err)
	}
	if config["use_ssl"] != true || config["smtp_port"] != float64(465) {
		t.Fatalf("stored email config = %#v", config)
	}

	recorder = test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"email":{"enabled":true,"use_ssl":"yes"}}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("wrong-type use_ssl status = %d, body = %s", recorder.Code, recorder.Body)
	}
}

func TestCardPolicyDefaultValidationAndPersistence(t *testing.T) {
	test := newSettingsAPITest(t)
	const iccid = "89860012345678901234"
	recorder := test.request(
		t,
		http.MethodGet,
		"/api/cards/"+iccid+"/policy",
		"",
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("default policy status = %d, body = %s", recorder.Code, recorder.Body)
	}
	response := decodeSettingsResponse(t, recorder)
	policy := response["data"].(map[string]any)
	if policy["iccid"] != iccid || policy["source"] != "default" ||
		policy["ip_version"] != "IPV4V6" {
		t.Fatalf("default policy = %#v", policy)
	}

	recorder = test.request(
		t,
		http.MethodPut,
		"/api/cards/"+iccid+"/policy",
		`{"vowifi_enabled":true,"airplane_enabled":true,"apn":"ims","ip_version":"IPV4V6"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("conflicting policy status = %d, body = %s", recorder.Code, recorder.Body)
	}

	recorder = test.request(
		t,
		http.MethodPut,
		"/api/cards/"+iccid+"/policy",
		`{"vowifi_enabled":true,"airplane_enabled":false,"apn":"ims","ip_version":"ipv4v6"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save policy status = %d, body = %s", recorder.Code, recorder.Body)
	}
	response = decodeSettingsResponse(t, recorder)
	policy = response["data"].(map[string]any)
	if policy["source"] != "manual" || policy["vowifi_enabled"] != true ||
		policy["ip_version"] != "IPV4V6" {
		t.Fatalf("saved policy = %#v", policy)
	}
	stored, err := test.database.CardPolicy(context.Background(), iccid)
	if err != nil || !stored.VoWiFiEnabled || stored.APN != "ims" {
		t.Fatalf("stored policy = %+v, %v", stored, err)
	}

	recorder = test.request(t, http.MethodGet, "/api/cards/not-an-iccid/policy", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid ICCID status = %d", recorder.Code)
	}
}

func TestTrafficAnalysisUsesAndAggregatesStoredBuckets(t *testing.T) {
	test := newSettingsAPITest(t)
	period := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	for _, bucket := range []store.TrafficBucket{
		{
			DeviceID: "ec20-1", Bucket: "day", PeriodStart: period,
			RXBytes: 100, TXBytes: 20,
		},
		{
			DeviceID: "ec20-2", Bucket: "day", PeriodStart: period,
			RXBytes: 50, TXBytes: 30,
		},
		{
			DeviceID: "ec20-1", Bucket: "week", PeriodStart: period,
			RXBytes: 9999, TXBytes: 9999,
		},
	} {
		if err := test.database.UpsertTrafficBucket(context.Background(), bucket); err != nil {
			t.Fatal(err)
		}
	}
	recorder := test.request(
		t,
		http.MethodGet,
		"/api/traffic/analysis?range=day",
		"",
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("traffic status = %d, body = %s", recorder.Code, recorder.Body)
	}
	response := decodeSettingsResponse(t, recorder)
	data := response["data"].(map[string]any)
	buckets := data["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("traffic buckets = %#v", buckets)
	}
	bucket := buckets[0].(map[string]any)
	if bucket["rx_bytes"] != float64(150) ||
		bucket["tx_bytes"] != float64(50) ||
		bucket["total_bytes"] != float64(200) {
		t.Fatalf("aggregated bucket = %#v", bucket)
	}

	recorder = test.request(
		t,
		http.MethodGet,
		"/api/traffic/analysis?range=year",
		"",
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid traffic range status = %d", recorder.Code)
	}
}

func TestNotificationDestinationAddressPolicy(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "10.0.0.1", "100.100.100.200", "127.0.0.1",
		"169.254.169.254", "172.16.0.1", "192.168.1.1", "198.18.0.1",
		"::1", "fc00::1", "fe80::1", "2001:db8::1",
	}
	for _, text := range blocked {
		address := netip.MustParseAddr(text)
		if publicNotificationAddress(address) {
			t.Errorf("%s was incorrectly accepted as public", text)
		}
	}
	for _, text := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		address := netip.MustParseAddr(text)
		if !publicNotificationAddress(address) {
			t.Errorf("%s was incorrectly blocked", text)
		}
	}
	if _, err := resolvePublicAddresses(context.Background(), "localhost"); err == nil {
		t.Fatal("localhost was not blocked")
	}
	if _, err := resolvePublicAddresses(
		context.Background(),
		"169.254.169.254",
	); err == nil {
		t.Fatal("metadata IP was not blocked")
	}
}

func TestRestrictedNotificationClientCapsTimeoutAndRedirects(t *testing.T) {
	client, err := restrictedHTTPClient(context.Background(), time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 10*time.Second {
		t.Fatalf("client timeout = %v", client.Timeout)
	}
	request := httptest.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err := client.CheckRedirect(request, nil); err == nil {
		t.Fatal("notification client followed a redirect")
	}
}

func TestRouteSettingsAPIReturnsFalseForUnknownPath(t *testing.T) {
	test := newSettingsAPITest(t)
	request := httptest.NewRequest(http.MethodGet, "/api/not-settings", nil)
	if test.server.routeSettingsAPI(httptest.NewRecorder(), request, "not-settings") {
		t.Fatal("unknown path was claimed by settings router")
	}
}
