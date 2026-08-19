package server

import (
	"strings"
	"testing"
	"time"
)

func TestIncomingCallNotificationTextFormatting(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)
	notification := IncomingCallNotification{
		DeviceID:    "ec20-1",
		DeviceName:  "Main Router",
		DeviceLabel: "Main Router",
		Caller:      "+8613800138000",
		Called:      "+8613900139000",
		Time:        now,
		Environment: "vowifi",
	}

	if notification.Title() != "收到来电" {
		t.Errorf("Title() = %q, want '收到来电'", notification.Title())
	}

	text := notification.Text()
	for _, want := range []string{
		"📞 收到来电",
		"设备  Main Router",
		"来电号码  +861380138000"[:10],
		"被呼号码  +8613900139000",
		"网络  VoWiFi",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() omitted %q:\n%s", want, text)
		}
	}

	notification.Environment = "cellular"
	if !strings.Contains(notification.Text(), "网络  基站直连") {
		t.Errorf("Text() in cellular mode omitted '网络  基站直连':\n%s", notification.Text())
	}
}

func TestIncomingCallDeduplication(t *testing.T) {
	now := time.Now()
	key := "test-device:+8613800000000"

	// First call should not be suppressed
	if shouldSuppressDuplicateCall(key, now, time.Minute) {
		t.Fatal("first call unexpectedly suppressed")
	}

	// Immediate duplicate should be suppressed
	if !shouldSuppressDuplicateCall(key, now.Add(5*time.Second), time.Minute) {
		t.Fatal("duplicate call within window was not suppressed")
	}

	// Call after window should be allowed
	if shouldSuppressDuplicateCall(key, now.Add(70*time.Second), time.Minute) {
		t.Fatal("call after window was suppressed")
	}
}

func TestRenderCallWebhookTemplate(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)
	message := IncomingCallNotification{
		DeviceID:    "dev-1",
		DeviceName:  "Living Room",
		DeviceLabel: "EC20",
		Caller:      "+8613800000000",
		Called:      "+8613900000000",
		Time:        now,
		Environment: "vowifi",
	}

	got := renderCallWebhookTemplate("{{event}}|{{device_id}}|{{device_name}}|{{device_label}}|{{caller}}|{{called}}|{{environment}}", message)
	want := "call.received|dev-1|Living Room|EC20|+8613800000000|+8613900000000|vowifi"
	if got != want {
		t.Fatalf("renderCallWebhookTemplate() = %q, want %q", got, want)
	}
}

func TestWecomAndLarkCallValues(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 20, 18, 0, 0, 0, location)
	message := IncomingCallNotification{
		DeviceID:    "dev-1",
		DeviceName:  "Office",
		DeviceLabel: "EC20-Office",
		Caller:      "+8613800138000",
		Called:      "+8613900139000",
		Time:        now,
		Environment: "cellular",
	}

	wecom := wecomCallValues(message)
	if wecom["event"] != "call.received" || wecom["title"] != "收到来电" || wecom["number"] != "+8613800138000" {
		t.Fatalf("wecomCallValues = %#v", wecom)
	}
	if !strings.Contains(wecom["message"], "网络  基站直连") {
		t.Fatalf("wecomCallValues message omitted network: %s", wecom["message"])
	}

	lark := larkCallValues(message)
	if lark["event"] != "call.received" || lark["title"] != "收到来电" || lark["device_label"] != "EC20-Office" {
		t.Fatalf("larkCallValues = %#v", lark)
	}
}
