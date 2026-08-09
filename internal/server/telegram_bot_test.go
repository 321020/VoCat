package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func TestTelegramAPIURLSupportsBaseAndTemplate(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "base URL",
			baseURL: "https://api.telegram.org",
			want:    "https://api.telegram.org/bot123456:test-token/sendMessage",
		},
		{
			name:    "reverse proxy template",
			baseURL: "https://telegram.example.com/bot%s/%s",
			want:    "https://telegram.example.com/bot123456:test-token/sendMessage",
		},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			got, err := telegramAPIURL(item.baseURL, "123456:test-token", "sendMessage")
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != item.want {
				t.Fatalf("telegramAPIURL() = %q, want %q", got, item.want)
			}
		})
	}
}

func TestTelegramAPIURLRejectsMalformedTemplates(t *testing.T) {
	for _, value := range []string{
		"https://telegram.example.com/bot%s/sendMessage",
		"https://%s.example.com/bot/token/%s",
		"http://telegram.example.com/bot%s/%s",
	} {
		if _, err := telegramAPIURL(value, "123456:test-token", "sendMessage"); err == nil {
			t.Errorf("telegramAPIURL(%q) unexpectedly succeeded", value)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("telegramAPIURL(%q) returned an empty error", value)
		}
	}
}

func TestParseTelegramCommand(t *testing.T) {
	command, remainder := parseTelegramCommand("  /sms@vocat_bot EC20 +447700900123 hello world  ")
	if command != "sms" || remainder != "EC20 +447700900123 hello world" {
		t.Fatalf("parseTelegramCommand() = %q, %q", command, remainder)
	}
	if command, _ := parseTelegramCommand("ordinary message"); command != "" {
		t.Fatalf("non-command parsed as %q", command)
	}
}

func TestSplitTelegramArgumentsPreservesMessageBody(t *testing.T) {
	parts := splitTelegramArguments("  EC20   +447700900123   code with spaces  ", 3)
	if len(parts) != 3 || parts[0] != "EC20" || parts[1] != "+447700900123" || parts[2] != "code with spaces" {
		t.Fatalf("splitTelegramArguments() = %#v", parts)
	}
}

func TestValidTelegramDialNumber(t *testing.T) {
	for _, value := range []string{"10086", "+447700900123", "12345678901234567890"} {
		if !validTelegramDialNumber(value) {
			t.Errorf("validTelegramDialNumber(%q) = false", value)
		}
	}
	for _, value := range []string{"12", "+", "123;ATH", "12 34", "123456789012345678901"} {
		if validTelegramDialNumber(value) {
			t.Errorf("validTelegramDialNumber(%q) = true", value)
		}
	}
}

func TestTelegramPendingActionIsAuthorizedOneShot(t *testing.T) {
	bot := &telegramBot{pending: make(map[string]telegramPendingAction)}
	action := telegramPendingAction{Kind: "call", ChatID: -1001, AdminID: 42, CreatedAt: time.Now()}
	token, err := bot.putPending(action)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bot.takePending(token, -1001, 41); ok {
		t.Fatal("different administrator consumed pending action")
	}
	if _, ok := bot.takePending(token, -1001, 42); ok {
		t.Fatal("an unauthorized attempt must invalidate the one-time action")
	}
}

func TestFormatTelegramATIncludesFinalResult(t *testing.T) {
	if got := formatTelegramAT(modem.Response{Final: "OK"}); got != "OK" {
		t.Fatalf("formatTelegramAT(OK) = %q", got)
	}
	if got := formatTelegramAT(modem.Response{Lines: []string{"+CLCC: 1"}, Final: "OK"}); got != "+CLCC: 1\nOK" {
		t.Fatalf("formatTelegramAT(lines) = %q", got)
	}
}

func TestTelegramExecutesGuardedATForConfiguredDevice(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "EC20", Name: "EC20"}); err != nil {
		t.Fatal(err)
	}
	bot := &telegramBot{server: &Server{
		store: database,
		devices: fakeDeviceController{
			entry:      device.Device{ID: "EC20", Discovered: true},
			atResponse: modem.Response{Lines: []string{"+CSQ: 18,99"}, Final: "OK"},
		},
	}}
	result, err := bot.executeATCommand(context.Background(), "EC20", "AT+CSQ")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"设备：EC20", "> AT+CSQ", "+CSQ: 18,99", "OK"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("AT result %q does not contain %q", result, expected)
		}
	}
	if _, err := bot.executeATCommand(context.Background(), "EC20", "AT+CFUN=0"); err == nil {
		t.Fatal("guarded AT command unexpectedly succeeded")
	}
}

func TestTelegramExecutesInteractiveUSSDForConfiguredDevice(t *testing.T) {
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.UpsertDevice(context.Background(), store.Device{ID: "EC20", Name: "EC20"}); err != nil {
		t.Fatal(err)
	}
	bot := &telegramBot{server: &Server{
		store: database,
		devices: fakeDeviceController{
			entry: device.Device{ID: "EC20", Discovered: true},
			ussdResult: device.USSDResult{
				Code: "*100#", Text: "1. Balance\n2. Bundles", Status: "awaiting_input",
				SessionID: "0123456789abcdef", Continueable: true,
			},
		},
	}}
	result, err := bot.executeUSSDCommand(context.Background(), "EC20", "*100#")
	if err != nil {
		t.Fatal(err)
	}
	formatted := formatTelegramUSSD("EC20", result)
	for _, expected := range []string{
		"设备：EC20", "状态：awaiting_input", "1. Balance", "/ussd_reply 0123456789abcdef", "/ussd_cancel 0123456789abcdef",
	} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("USSD result %q does not contain %q", formatted, expected)
		}
	}
}
