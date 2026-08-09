package server

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
	"vocat/internal/vowifi"
	vowifiruntime "vocat/internal/vowifi/runtime"
)

const (
	telegramPollInterval       = 3 * time.Second
	telegramNotificationPeriod = 2 * time.Second
	telegramConfirmationTTL    = 2 * time.Minute
	telegramMaxDialDuration    = 10 * time.Minute
)

var telegramTokenInURLPattern = regexp.MustCompile(`bot[0-9]{5,20}:[A-Za-z0-9_-]{20,128}`)

type telegramRuntimeConfig struct {
	Token   string
	ChatID  string
	AdminID int64
	BaseURL string
	Proxy   string
}

type telegramBot struct {
	server *Server

	pendingMu sync.Mutex
	pending   map[string]telegramPendingAction

	logMu       sync.Mutex
	lastLogTime time.Time
	lastLogText string
}

type telegramPendingAction struct {
	Kind        string
	DeviceID    string
	Argument    string
	Text        string
	Duration    time.Duration
	ChatID      int64
	AdminID     int64
	CreatedAt   time.Time
	TargetAID   string
	TargetICCID string
}

type telegramAPIResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat  `json:"chat"`
	Text      string        `json:"text"`
}

type telegramUser struct {
	ID int64 `json:"id"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

// StartTelegramBot starts both the Telegram command poller and durable inbound
// SMS notifier. Configuration is reloaded between polls, so saving Settings
// takes effect without restarting vocat.
func (s *Server) StartTelegramBot(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	bot := &telegramBot{
		server:  s,
		pending: make(map[string]telegramPendingAction),
	}
	go bot.poll(ctx)
	go bot.notifyInboundSMS(ctx)
}

func (bot *telegramBot) poll(ctx context.Context) {
	activeToken := ""
	var offset int64
	for ctx.Err() == nil {
		config, enabled, err := bot.loadConfig(ctx)
		if err != nil {
			bot.warn("load Telegram bot configuration", err)
			if !waitTelegram(ctx, telegramPollInterval) {
				return
			}
			continue
		}
		if !enabled {
			activeToken = ""
			offset = 0
			if !waitTelegram(ctx, telegramPollInterval) {
				return
			}
			continue
		}
		if config.Token != activeToken {
			offset, err = bot.bootstrap(ctx, config)
			if err != nil {
				bot.warn("start Telegram bot polling", err)
				if !waitTelegram(ctx, telegramPollInterval) {
					return
				}
				continue
			}
			activeToken = config.Token
		}
		pollContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		updates, pollErr := bot.getUpdates(pollContext, config, offset, 5)
		cancel()
		if pollErr != nil {
			if ctx.Err() != nil {
				return
			}
			bot.warn("poll Telegram updates", pollErr)
			if !waitTelegram(ctx, telegramPollInterval) {
				return
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			update := update
			go bot.handleUpdate(ctx, config, update)
		}
	}
}

// bootstrap discards stale Telegram updates. Replaying an old /sms, /call, or
// /switch command after a service restart would be unsafe even though each
// command has its own confirmation step.
func (bot *telegramBot) bootstrap(ctx context.Context, config telegramRuntimeConfig) (int64, error) {
	requestContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	updates, err := bot.getUpdates(requestContext, config, -1, 0)
	if err != nil {
		return 0, err
	}
	var offset int64
	for _, update := range updates {
		if update.UpdateID >= offset {
			offset = update.UpdateID + 1
		}
	}
	commands := []map[string]string{
		{"command": "status", "description": "查看设备状态"},
		{"command": "esim", "description": "查看已安装 eSIM Profile"},
		{"command": "wfc", "description": "管理 WiFi Calling"},
		{"command": "sms", "description": "发送短信（需要确认）"},
		{"command": "call", "description": "限时拨号并自动挂断（需要确认）"},
		{"command": "calls", "description": "查看当前通话"},
		{"command": "hangup", "description": "挂断通话"},
		{"command": "at", "description": "向指定设备发送安全 AT 指令"},
		{"command": "ussd", "description": "向指定设备发送 USSD 指令"},
		{"command": "ussd_reply", "description": "回复交互式 USSD 会话"},
		{"command": "ussd_cancel", "description": "取消交互式 USSD 会话"},
		{"command": "help", "description": "查看命令帮助"},
	}
	_ = bot.call(requestContext, config, "setMyCommands", map[string]any{"commands": commands}, nil)
	return offset, nil
}

func (bot *telegramBot) getUpdates(
	ctx context.Context,
	config telegramRuntimeConfig,
	offset int64,
	timeout int,
) ([]telegramUpdate, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}
	var updates []telegramUpdate
	if err := bot.call(ctx, config, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (bot *telegramBot) handleUpdate(ctx context.Context, config telegramRuntimeConfig, update telegramUpdate) {
	if callback := update.CallbackQuery; callback != nil {
		if callback.Message == nil || !bot.authorized(config, callback.Message.Chat.ID, callback.From.ID) {
			_ = bot.answerCallback(ctx, config, callback.ID, "无权限")
			return
		}
		_ = bot.answerCallback(ctx, config, callback.ID, "")
		bot.handleCallback(ctx, config, callback)
		return
	}
	message := update.Message
	if message == nil || message.From == nil || !bot.authorized(config, message.Chat.ID, message.From.ID) {
		return
	}
	command, remainder := parseTelegramCommand(message.Text)
	if command == "" {
		return
	}
	switch command {
	case "start", "menu", "help":
		bot.sendHelp(ctx, config, message.Chat.ID)
	case "status", "devices":
		bot.sendDeviceStatus(ctx, config, message.Chat.ID, strings.TrimSpace(remainder))
	case "esim":
		bot.sendESIMProfiles(ctx, config, message.Chat.ID, strings.TrimSpace(remainder))
	case "switch":
		parts := strings.Fields(remainder)
		if len(parts) != 2 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/switch <设备ID> <目标ICCID>", nil)
			return
		}
		bot.confirmESIMSwitch(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1])
	case "wfc", "wificalling":
		parts := strings.Fields(remainder)
		if len(parts) != 2 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/wfc <设备ID> <status|on|off|reconnect>", nil)
			return
		}
		bot.handleVoWiFi(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1])
	case "sms":
		parts := splitTelegramArguments(remainder, 3)
		if len(parts) != 3 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/sms <设备ID> <号码> <短信内容>", nil)
			return
		}
		bot.confirmSMS(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1], parts[2])
	case "call":
		parts := strings.Fields(remainder)
		if len(parts) != 3 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/call <设备ID> <号码> <持续秒数>\n拨号后将在指定时间自动挂断，不处理通话音频。", nil)
			return
		}
		seconds, err := strconv.Atoi(parts[2])
		if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > telegramMaxDialDuration {
			bot.sendText(ctx, config, message.Chat.ID, "持续时间必须是 1–600 秒。", nil)
			return
		}
		bot.confirmCall(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1], time.Duration(seconds)*time.Second)
	case "answer":
		bot.executeSimpleCallAction(ctx, config, message.Chat.ID, message.From.ID, strings.TrimSpace(remainder), "answer")
	case "hangup":
		bot.executeSimpleCallAction(ctx, config, message.Chat.ID, message.From.ID, strings.TrimSpace(remainder), "hangup")
	case "calls":
		bot.executeSimpleCallAction(ctx, config, message.Chat.ID, message.From.ID, strings.TrimSpace(remainder), "status")
	case "at":
		parts := splitTelegramArguments(remainder, 2)
		if len(parts) != 2 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/at <设备ID> <AT指令>\n示例：/at EC20 AT+CSQ", nil)
			return
		}
		bot.handleATCommand(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1])
	case "ussd":
		parts := strings.Fields(remainder)
		if len(parts) != 2 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/ussd <设备ID> <USSD代码>\n示例：/ussd EC20 *100#", nil)
			return
		}
		bot.handleUSSDCommand(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1])
	case "ussd_reply":
		parts := splitTelegramArguments(remainder, 2)
		if len(parts) != 2 {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/ussd_reply <会话ID> <回复内容>", nil)
			return
		}
		bot.handleUSSDReply(ctx, config, message.Chat.ID, message.From.ID, parts[0], parts[1])
	case "ussd_cancel":
		sessionID := strings.TrimSpace(remainder)
		if sessionID == "" || strings.ContainsAny(sessionID, " \t\r\n") {
			bot.sendText(ctx, config, message.Chat.ID, "用法：/ussd_cancel <会话ID>", nil)
			return
		}
		bot.handleUSSDCancel(ctx, config, message.Chat.ID, message.From.ID, sessionID)
	default:
		bot.sendText(ctx, config, message.Chat.ID, "未知命令。发送 /help 查看可用操作。", nil)
	}
}

func (bot *telegramBot) handleCallback(ctx context.Context, config telegramRuntimeConfig, callback *telegramCallbackQuery) {
	data := strings.TrimSpace(callback.Data)
	if data == "menu:status" {
		bot.sendDeviceStatus(ctx, config, callback.Message.Chat.ID, "")
		return
	}
	if data == "menu:help" {
		bot.sendHelp(ctx, config, callback.Message.Chat.ID)
		return
	}
	decision, token, found := strings.Cut(data, ":")
	if !found || (decision != "confirm" && decision != "cancel") {
		return
	}
	action, ok := bot.takePending(token, callback.Message.Chat.ID, callback.From.ID)
	if !ok {
		bot.sendText(ctx, config, callback.Message.Chat.ID, "该确认已过期或已处理。", nil)
		return
	}
	if decision == "cancel" {
		bot.sendText(ctx, config, callback.Message.Chat.ID, "操作已取消。", nil)
		return
	}
	switch action.Kind {
	case "sms":
		bot.sendText(ctx, config, action.ChatID, "正在提交短信…", nil)
		result, err := bot.executeSMS(ctx, action)
		bot.finishAction(ctx, config, action, "telegram.sms.send", result, err)
	case "esim_switch":
		bot.sendText(ctx, config, action.ChatID, "正在切换 Profile 并等待模块恢复校验…", nil)
		result, err := bot.executeESIMSwitch(ctx, action)
		bot.finishAction(ctx, config, action, "telegram.esim.switch", result, err)
	case "call":
		result, err := bot.executeTimedCall(ctx, config, action)
		bot.finishAction(ctx, config, action, "telegram.call.dial", result, err)
	}
}

func (bot *telegramBot) sendHelp(ctx context.Context, config telegramRuntimeConfig, chatID int64) {
	text := strings.Join([]string{
		"vocat Telegram 控制", "",
		"/status [设备ID] — 查看设备、SIM、蜂窝与 VoWiFi 状态",
		"/esim <设备ID> — 只读查看已安装 Profile",
		"/switch <设备ID> <ICCID> — 切换到已安装 Profile（需确认）",
		"/wfc <设备ID> <status|on|off|reconnect> — 管理 WiFi Calling",
		"/sms <设备ID> <号码> <内容> — 发送短信（需确认）",
		"/call <设备ID> <号码> <秒数> — 拨号并在 1–600 秒后自动挂断（需确认）",
		"/calls <设备ID> — 查看模块当前通话",
		"/answer <设备ID> — 接听蜂窝来电",
		"/hangup <设备ID> — 立即挂断",
		"/at <设备ID> <AT指令> — 执行经过安全校验的单行 AT 指令",
		"/ussd <设备ID> <代码> — 发送 USSD 指令",
		"/ussd_reply <会话ID> <内容> — 回复交互式 USSD 菜单",
		"/ussd_cancel <会话ID> — 取消交互式 USSD 会话",
		"",
		"Bot 不提供 eSIM 下载、删除或改名，也不采集或转发通话音频。控制命令只接受设置中的 Admin ID。",
	}, "\n")
	keyboard := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "📊 设备状态", "callback_data": "menu:status"},
		{"text": "❓ 帮助", "callback_data": "menu:help"},
	}}}
	bot.sendText(ctx, config, chatID, text, keyboard)
}

func (bot *telegramBot) sendDeviceStatus(ctx context.Context, config telegramRuntimeConfig, chatID int64, onlyID string) {
	configs, err := bot.server.store.ListDevices(ctx)
	if err != nil {
		bot.sendText(ctx, config, chatID, "读取设备失败："+err.Error(), nil)
		return
	}
	var blocks []string
	for _, stored := range configs {
		if onlyID != "" && stored.ID != onlyID {
			continue
		}
		entry, _, present := bot.server.physicalForConfig(stored)
		lines := []string{fmt.Sprintf("📡 %s (%s)", firstNonEmpty(stored.Name, stored.ID), stored.ID)}
		if !present {
			lines = append(lines, "设备：离线")
		} else {
			lines = append(lines, "设备：在线")
			if snapshot := entry.Snapshot; snapshot != nil {
				lines = append(lines,
					"SIM："+map[bool]string{true: "Ready", false: firstNonEmpty(snapshot.SIMStatus, "未就绪")}[snapshot.SIMReady],
					"ICCID："+firstNonEmpty(snapshot.ICCID, "--"),
					"IMSI："+firstNonEmpty(snapshot.IMSI, "--"),
					"号码："+firstNonEmpty(snapshot.Phone.Number, "--"),
					"运营商："+firstNonEmpty(snapshot.OperatorName, snapshot.OperatorCode, "--"),
					"蜂窝模式："+map[bool]string{true: "飞行模式", false: "开启"}[snapshot.FlightMode],
				)
			}
		}
		if bot.server.vowifi != nil {
			if state, stateErr := bot.server.vowifi.State(stored.ID); stateErr == nil {
				lines = append(lines,
					fmt.Sprintf("VoWiFi：%s · Tunnel=%t IMS=%t SMS=%t", firstNonEmpty(string(state.Phase), "idle"), state.TunnelReady, state.IMSReady, state.SMSReady),
				)
				if state.LastError != "" {
					lines = append(lines, "最后错误："+state.LastError)
				}
			}
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if len(blocks) == 0 {
		bot.sendText(ctx, config, chatID, "未找到设备 "+onlyID, nil)
		return
	}
	bot.sendText(ctx, config, chatID, strings.Join(blocks, "\n\n"), nil)
}

func (bot *telegramBot) sendESIMProfiles(ctx context.Context, config telegramRuntimeConfig, chatID int64, deviceID string) {
	if deviceID == "" {
		bot.sendText(ctx, config, chatID, "用法：/esim <设备ID>", nil)
		return
	}
	_, _, physicalID, err := bot.device(deviceID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "读取 eSIM 失败："+err.Error(), nil)
		return
	}
	readContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inventory, err := bot.server.devices.ESIMInventory(readContext, physicalID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "读取 eSIM 失败："+err.Error(), nil)
		return
	}
	if len(inventory) == 0 {
		bot.sendText(ctx, config, chatID, "该设备没有可用的 eUICC/Profile。", nil)
		return
	}
	lines := []string{"📲 " + deviceID + " 已安装 Profile（只读）"}
	for index, group := range inventory {
		lines = append(lines, fmt.Sprintf("\neUICC #%d · …%s", index+1, tailDigits(group.Info.EID, 4)))
		for _, profile := range group.Info.Profiles {
			state := "Disabled"
			if profile.State == 1 {
				state = "Enabled"
			}
			name := firstNonEmpty(profile.Nickname, profile.Name, profile.ServiceProvider, "未命名")
			lines = append(lines, fmt.Sprintf("• %s · %s\n  %s", name, state, profile.ICCID))
		}
	}
	lines = append(lines, "\n切换：/switch "+deviceID+" <目标ICCID>")
	bot.sendText(ctx, config, chatID, strings.Join(lines, "\n"), nil)
}

func (bot *telegramBot) confirmESIMSwitch(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, iccid string) {
	_, _, physicalID, err := bot.device(deviceID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "无法切换："+err.Error(), nil)
		return
	}
	readContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inventory, err := bot.server.devices.ESIMInventory(readContext, physicalID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "无法读取 Profile："+err.Error(), nil)
		return
	}
	var target *device.EsimProfile
	var targetAID string
	for groupIndex := range inventory {
		for profileIndex := range inventory[groupIndex].Info.Profiles {
			profile := &inventory[groupIndex].Info.Profiles[profileIndex]
			if profile.ICCID == iccid {
				target = profile
				targetAID = inventory[groupIndex].Info.AID
				break
			}
		}
	}
	if target == nil {
		bot.sendText(ctx, config, chatID, "目标 ICCID 不在该设备已安装 Profile 中。", nil)
		return
	}
	if target.State == 1 {
		bot.sendText(ctx, config, chatID, "目标 Profile 已经处于 Enabled。", nil)
		return
	}
	action := telegramPendingAction{
		Kind: "esim_switch", DeviceID: deviceID, ChatID: chatID, AdminID: adminID,
		CreatedAt: time.Now(), TargetAID: targetAID, TargetICCID: target.ICCID,
	}
	name := firstNonEmpty(target.Nickname, target.Name, target.ServiceProvider, "未命名")
	bot.askConfirmation(ctx, config, action, fmt.Sprintf("确认将设备 %s 切换到：\n%s\nICCID %s？\n\nBot 只会执行 EnableProfile，不会下载或删除 Profile。", deviceID, name, target.ICCID))
}

func (bot *telegramBot) confirmSMS(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, phone, text string) {
	phone = strings.TrimSpace(phone)
	text = strings.TrimSpace(text)
	if _, _, _, err := bot.device(deviceID); err != nil {
		bot.sendText(ctx, config, chatID, "无法发送："+err.Error(), nil)
		return
	}
	if blocked, reason := blockedSMSDestination(phone); blocked {
		bot.sendText(ctx, config, chatID, "无法发送："+reason, nil)
		return
	}
	if text == "" {
		bot.sendText(ctx, config, chatID, "短信内容不能为空。", nil)
		return
	}
	action := telegramPendingAction{
		Kind: "sms", DeviceID: deviceID, Argument: phone, Text: text,
		ChatID: chatID, AdminID: adminID, CreatedAt: time.Now(),
	}
	bot.askConfirmation(ctx, config, action, fmt.Sprintf("确认通过设备 %s 发送短信？\n收件人：%s\n内容：%s", deviceID, phone, truncateTelegramText(text, 800)))
}

func (bot *telegramBot) confirmCall(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, number string, duration time.Duration) {
	if !validTelegramDialNumber(number) {
		bot.sendText(ctx, config, chatID, "拨号号码无效，只允许一个可选的前导 + 和 3–20 位数字。", nil)
		return
	}
	if _, entry, _, err := bot.device(deviceID); err != nil {
		bot.sendText(ctx, config, chatID, "无法拨号："+err.Error(), nil)
		return
	} else if entry.Snapshot != nil && entry.Snapshot.FlightMode {
		bot.sendText(ctx, config, chatID, "设备处于飞行模式，蜂窝语音拨号不可用。当前 Bot 不实现 IMS 语音或音频处理。", nil)
		return
	}
	action := telegramPendingAction{
		Kind: "call", DeviceID: deviceID, Argument: number, Duration: duration,
		ChatID: chatID, AdminID: adminID, CreatedAt: time.Now(),
	}
	bot.askConfirmation(ctx, config, action, fmt.Sprintf("确认通过设备 %s 拨打 %s？\n持续：%d 秒，然后自动挂断。\n不会采集或处理通话音频。", deviceID, number, int(duration/time.Second)))
}

func (bot *telegramBot) askConfirmation(ctx context.Context, config telegramRuntimeConfig, action telegramPendingAction, text string) {
	token, err := bot.putPending(action)
	if err != nil {
		bot.sendText(ctx, config, action.ChatID, "创建确认失败："+err.Error(), nil)
		return
	}
	keyboard := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ 确认", "callback_data": "confirm:" + token},
		{"text": "❌ 取消", "callback_data": "cancel:" + token},
	}}}
	bot.sendText(ctx, config, action.ChatID, text, keyboard)
}

func (bot *telegramBot) putPending(action telegramPendingAction) (string, error) {
	raw := make([]byte, 8)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	bot.pendingMu.Lock()
	defer bot.pendingMu.Unlock()
	now := time.Now()
	for key, value := range bot.pending {
		if now.Sub(value.CreatedAt) > telegramConfirmationTTL {
			delete(bot.pending, key)
		}
	}
	bot.pending[token] = action
	return token, nil
}

func (bot *telegramBot) takePending(token string, chatID, adminID int64) (telegramPendingAction, bool) {
	bot.pendingMu.Lock()
	defer bot.pendingMu.Unlock()
	action, ok := bot.pending[token]
	if ok {
		delete(bot.pending, token)
	}
	if !ok || action.ChatID != chatID || action.AdminID != adminID || time.Since(action.CreatedAt) > telegramConfirmationTTL {
		return telegramPendingAction{}, false
	}
	return action, true
}

func (bot *telegramBot) executeSMS(ctx context.Context, action telegramPendingAction) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"device_id": action.DeviceID,
		"phone":     action.Argument,
		"message":   action.Text,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/sms/send", bytes.NewReader(payload)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	bot.server.handleSMSSend(recorder, request)
	var response struct {
		Data  map[string]any `json:"data"`
		Error *apiError      `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		return "", fmt.Errorf("decode SMS result: %w", err)
	}
	if recorder.Code >= http.StatusBadRequest || response.Error != nil {
		if response.Error != nil {
			return "", errors.New(response.Error.Message)
		}
		return "", fmt.Errorf("SMS submission returned HTTP %d", recorder.Code)
	}
	return fmt.Sprintf("短信已提交。\n通道：%v\n结果：%v\n送达确认：%v", response.Data["transport"], response.Data["outcome"], response.Data["delivery_confirmed"]), nil
}

func (bot *telegramBot) executeESIMSwitch(ctx context.Context, action telegramPendingAction) (string, error) {
	_, _, physicalID, err := bot.device(action.DeviceID)
	if err != nil {
		return "", err
	}
	operationContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := bot.server.devices.ESIMSwitchProfile(operationContext, physicalID, action.TargetICCID, action.TargetAID); err != nil {
		return "", err
	}
	return "Profile 切换成功，模块恢复后已校验当前 ICCID：" + action.TargetICCID, nil
}

func (bot *telegramBot) executeTimedCall(ctx context.Context, config telegramRuntimeConfig, action telegramPendingAction) (string, error) {
	_, entry, physicalID, err := bot.device(action.DeviceID)
	if err != nil {
		return "", err
	}
	if entry.Snapshot != nil && entry.Snapshot.FlightMode {
		return "", errors.New("device is in airplane mode")
	}
	dialContext, cancelDial := context.WithTimeout(ctx, 20*time.Second)
	response, err := bot.server.devices.ExecuteAT(dialContext, physicalID, "ATD"+action.Argument+";")
	cancelDial()
	if err != nil {
		return "", fmt.Errorf("拨号失败: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(response.Final), "OK") {
		return "", fmt.Errorf("拨号未被模块接受: %s", formatTelegramAT(response))
	}
	bot.sendText(ctx, config, action.ChatID, fmt.Sprintf("📞 已开始拨打 %s，将在 %d 秒后自动挂断。", action.Argument, int(action.Duration/time.Second)), nil)
	timer := time.NewTimer(action.Duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	}
	hangContext, cancelHang := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelHang()
	hangResponse, hangErr := bot.server.devices.ExecuteAT(hangContext, physicalID, "ATH")
	if hangErr != nil {
		return "", fmt.Errorf("拨号已执行，但自动挂断失败: %w", hangErr)
	}
	if !strings.EqualFold(strings.TrimSpace(hangResponse.Final), "OK") {
		return "", fmt.Errorf("拨号已执行，但模块未确认自动挂断: %s", formatTelegramAT(hangResponse))
	}
	return fmt.Sprintf("拨号动作完成：%s，持续 %d 秒后已自动挂断。", action.Argument, int(action.Duration/time.Second)), nil
}

func (bot *telegramBot) executeSimpleCallAction(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, action string) {
	if deviceID == "" {
		bot.sendText(ctx, config, chatID, fmt.Sprintf("用法：/%s <设备ID>", map[string]string{"status": "calls", "answer": "answer", "hangup": "hangup"}[action]), nil)
		return
	}
	_, _, physicalID, err := bot.device(deviceID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "通话操作失败："+err.Error(), nil)
		return
	}
	command := map[string]string{"status": "AT+CLCC", "answer": "ATA", "hangup": "ATH"}[action]
	operationContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	response, err := bot.server.devices.ExecuteAT(operationContext, physicalID, command)
	cancel()
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "通话操作失败："+err.Error(), nil)
	} else {
		text := formatTelegramAT(response)
		if action == "status" && strings.TrimSpace(response.Text()) == "" {
			text = "当前没有活动通话。"
		}
		bot.sendText(ctx, config, chatID, text, nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.call."+action, "device", deviceID, outcome, "telegram")
}

func (bot *telegramBot) handleATCommand(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, command string) {
	result, err := bot.executeATCommand(ctx, deviceID, command)
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "AT 指令执行失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, chatID, result, nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.at.execute", "device", deviceID, outcome, "telegram")
}

func (bot *telegramBot) executeATCommand(ctx context.Context, deviceID, command string) (string, error) {
	command = strings.TrimSpace(command)
	if err := validateATCommand(command); err != nil {
		return "", err
	}
	_, _, physicalID, err := bot.device(deviceID)
	if err != nil {
		return "", err
	}
	operationContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	response, err := bot.server.devices.ExecuteAT(operationContext, physicalID, command)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("设备：%s\n> %s\n\n%s", deviceID, command, formatTelegramAT(response)), nil
}

func (bot *telegramBot) handleUSSDCommand(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, code string) {
	result, err := bot.executeUSSDCommand(ctx, deviceID, code)
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "USSD 指令执行失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, chatID, formatTelegramUSSD(deviceID, result), nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.ussd.start", "device", deviceID, outcome, "telegram")
}

func (bot *telegramBot) executeUSSDCommand(ctx context.Context, deviceID, code string) (device.USSDResult, error) {
	_, _, physicalID, err := bot.device(deviceID)
	if err != nil {
		return device.USSDResult{}, err
	}
	operationContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return bot.server.devices.USSD(operationContext, physicalID, strings.TrimSpace(code))
}

func (bot *telegramBot) handleUSSDReply(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, sessionID, input string) {
	operationContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	result, err := bot.server.devices.ContinueUSSD(operationContext, strings.TrimSpace(sessionID), strings.TrimSpace(input))
	cancel()
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "USSD 回复失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, chatID, formatTelegramUSSD("", result), nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.ussd.reply", "ussd_session", "interactive", outcome, "telegram")
}

func (bot *telegramBot) handleUSSDCancel(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, sessionID string) {
	operationContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := bot.server.devices.CancelUSSD(operationContext, strings.TrimSpace(sessionID))
	cancel()
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "取消 USSD 会话失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, chatID, "USSD 会话已取消。", nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.ussd.cancel", "ussd_session", "interactive", outcome, "telegram")
}

func formatTelegramUSSD(deviceID string, result device.USSDResult) string {
	lines := make([]string, 0, 7)
	if strings.TrimSpace(deviceID) != "" {
		lines = append(lines, "设备："+strings.TrimSpace(deviceID))
	}
	if strings.TrimSpace(result.Code) != "" {
		lines = append(lines, "USSD："+strings.TrimSpace(result.Code))
	}
	lines = append(lines, "状态："+firstNonEmpty(strings.TrimSpace(result.Status), "final"))
	if strings.TrimSpace(result.Text) != "" {
		lines = append(lines, "\n"+strings.TrimSpace(result.Text))
	} else if strings.TrimSpace(result.Raw) != "" {
		lines = append(lines, "\n"+strings.TrimSpace(result.Raw))
	} else {
		lines = append(lines, "\n网络未返回文本内容。")
	}
	if result.Continueable && strings.TrimSpace(result.SessionID) != "" {
		lines = append(lines,
			"\n网络正在等待输入。",
			"回复：/ussd_reply "+result.SessionID+" <内容>",
			"取消：/ussd_cancel "+result.SessionID,
		)
	}
	return strings.Join(lines, "\n")
}

func (bot *telegramBot) handleVoWiFi(ctx context.Context, config telegramRuntimeConfig, chatID, adminID int64, deviceID, operation string) {
	stored, entry, _, err := bot.device(deviceID)
	if err != nil {
		bot.sendText(ctx, config, chatID, "VoWiFi 操作失败："+err.Error(), nil)
		return
	}
	if bot.server.vowifi == nil {
		bot.sendText(ctx, config, chatID, "VoWiFi runtime 不可用。", nil)
		return
	}
	operation = strings.ToLower(strings.TrimSpace(operation))
	if operation == "status" {
		state, stateErr := bot.server.vowifi.State(deviceID)
		if stateErr != nil {
			bot.sendText(ctx, config, chatID, "读取 VoWiFi 状态失败："+stateErr.Error(), nil)
			return
		}
		bot.sendText(ctx, config, chatID, formatTelegramVoWiFiState(state), nil)
		return
	}
	var state vowifi.State
	switch operation {
	case "on", "off":
		enabled := operation == "on"
		if enabled && entry.Snapshot != nil {
			if reason := device.RegionBlockReason(entry.Snapshot.IMSI); reason != "" {
				bot.sendText(ctx, config, chatID, "VoWiFi 操作被拒绝："+reason, nil)
				return
			}
		}
		previous := stored.VoWiFiEnabled
		stored.VoWiFiEnabled = enabled
		if err = bot.server.store.UpsertDevice(ctx, stored); err == nil {
			state, err = bot.server.vowifi.RequestEnabled(deviceID, enabled)
		}
		if err != nil {
			stored.VoWiFiEnabled = previous
			_ = bot.server.store.UpsertDevice(ctx, stored)
			if errors.Is(err, vowifiruntime.ErrOperationInProgress) && state.Enabled == enabled {
				err = nil
			}
		}
	case "reconnect":
		if !stored.VoWiFiEnabled {
			err = errors.New("请先启用 VoWiFi")
		} else {
			state, err = bot.server.vowifi.RequestReconnect(deviceID)
		}
	default:
		bot.sendText(ctx, config, chatID, "操作必须是 status、on、off 或 reconnect。", nil)
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, chatID, "VoWiFi 操作失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, chatID, "VoWiFi 操作已受理。\n"+formatTelegramVoWiFiState(state), nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", adminID), "telegram.vowifi."+operation, "device", deviceID, outcome, "telegram")
}

func (bot *telegramBot) finishAction(ctx context.Context, config telegramRuntimeConfig, action telegramPendingAction, auditAction, result string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
		bot.sendText(ctx, config, action.ChatID, "操作失败："+err.Error(), nil)
	} else {
		bot.sendText(ctx, config, action.ChatID, "✅ "+result, nil)
	}
	bot.server.recordAudit(ctx, fmt.Sprintf("telegram:%d", action.AdminID), auditAction, "device", action.DeviceID, outcome, "telegram")
}

func (bot *telegramBot) notifyInboundSMS(ctx context.Context) {
	cursorInitialized := false
	var cursor int64
	for ctx.Err() == nil {
		if !cursorInitialized {
			latest, err := bot.server.store.LatestSMSMessageID(ctx)
			if err != nil {
				bot.warn("initialize Telegram SMS cursor", err)
				if !waitTelegram(ctx, telegramNotificationPeriod) {
					return
				}
				continue
			}
			cursor, cursorInitialized = latest, true
		}
		config, enabled, err := bot.loadConfig(ctx)
		if err != nil {
			bot.warn("load Telegram SMS notification configuration", err)
		} else if !enabled {
			if latest, latestErr := bot.server.store.LatestSMSMessageID(ctx); latestErr == nil {
				cursor = latest
			}
		} else {
			messages, listErr := bot.server.store.ListInboundSMSAfterID(ctx, cursor, 100)
			if listErr != nil {
				bot.warn("list Telegram SMS notifications", listErr)
			} else {
				for _, message := range messages {
					text := fmt.Sprintf("📩 新短信\n设备：%s\n来自：%s\n时间：%s\n\n%s", message.DeviceID, message.Peer, message.Timestamp.Local().Format("2006-01-02 15:04:05"), message.Body)
					if sendErr := bot.sendText(ctx, config, 0, text, nil); sendErr != nil {
						bot.warn("send Telegram SMS notification", sendErr)
						break
					}
					cursor = message.ID
				}
			}
		}
		if !waitTelegram(ctx, telegramNotificationPeriod) {
			return
		}
	}
}

func (bot *telegramBot) device(deviceID string) (store.Device, device.Device, string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return store.Device{}, device.Device{}, "", errors.New("设备 ID 不能为空")
	}
	stored, err := bot.server.store.Device(context.Background(), deviceID)
	if err != nil {
		return store.Device{}, device.Device{}, "", err
	}
	entry, physicalID, present := bot.server.physicalForConfig(stored)
	if !present {
		return stored, entry, "", errors.New("设备不在线")
	}
	return stored, entry, physicalID, nil
}

func (bot *telegramBot) authorized(config telegramRuntimeConfig, chatID, userID int64) bool {
	return config.AdminID > 0 && userID == config.AdminID && strconv.FormatInt(chatID, 10) == config.ChatID
}

func (bot *telegramBot) loadConfig(ctx context.Context) (telegramRuntimeConfig, bool, error) {
	setting, err := bot.server.store.NotificationSetting(ctx, "telegram")
	if errors.Is(err, store.ErrNotFound) {
		return telegramRuntimeConfig{}, false, nil
	}
	if err != nil {
		return telegramRuntimeConfig{}, false, err
	}
	if !setting.Enabled {
		return telegramRuntimeConfig{}, false, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(setting.Config, &raw); err != nil {
		return telegramRuntimeConfig{}, false, fmt.Errorf("decode Telegram config: %w", err)
	}
	config := telegramRuntimeConfig{
		Token:   configString(raw, "bot_token"),
		ChatID:  configString(raw, "chat_id"),
		BaseURL: configString(raw, "base_url"),
		Proxy:   configString(raw, "proxy"),
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultTelegramBaseURL
	}
	if admin := configString(raw, "admin_id"); admin != "" {
		config.AdminID, err = strconv.ParseInt(admin, 10, 64)
		if err != nil || config.AdminID <= 0 {
			return telegramRuntimeConfig{}, false, errors.New("telegram.admin_id must be a positive integer")
		}
	}
	if !telegramTokenPattern.MatchString(config.Token) || config.ChatID == "" {
		return telegramRuntimeConfig{}, false, errors.New("Telegram bot token or chat id is invalid")
	}
	return config, true, nil
}

func (bot *telegramBot) call(ctx context.Context, config telegramRuntimeConfig, method string, payload any, result any) error {
	base, err := validateTelegramAPIURL(ctx, config.BaseURL, config.Token, method)
	if err != nil {
		return redactTelegramError(err, config.Token)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client, err := restrictedHTTPClient(ctx, 10*time.Second, config.Proxy)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return redactTelegramError(err, config.Token)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "vocat-telegram-bot/1")
	response, err := client.Do(request)
	if err != nil {
		return redactTelegramError(err, config.Token)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	var envelope telegramAPIResponse
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode Telegram response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		return fmt.Errorf("Telegram %s failed: HTTP %d %s", method, response.StatusCode, envelope.Description)
	}
	if result != nil && len(envelope.Result) != 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode Telegram %s result: %w", method, err)
		}
	}
	return nil
}

func (bot *telegramBot) sendText(ctx context.Context, config telegramRuntimeConfig, chatID int64, text string, replyMarkup any) error {
	target := config.ChatID
	if chatID != 0 {
		target = strconv.FormatInt(chatID, 10)
	}
	payload := map[string]any{
		"chat_id": target,
		"text":    truncateTelegramText(text, 3900),
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return bot.call(requestContext, config, "sendMessage", payload, nil)
}

func (bot *telegramBot) answerCallback(ctx context.Context, config telegramRuntimeConfig, callbackID, text string) error {
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	requestContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return bot.call(requestContext, config, "answerCallbackQuery", payload, nil)
}

func (bot *telegramBot) warn(message string, err error) {
	if err == nil || bot.server.logger == nil {
		return
	}
	now := time.Now()
	text := redactTelegramText(err.Error(), "")
	bot.logMu.Lock()
	if text == bot.lastLogText && now.Sub(bot.lastLogTime) < time.Minute {
		bot.logMu.Unlock()
		return
	}
	bot.lastLogText, bot.lastLogTime = text, now
	bot.logMu.Unlock()
	bot.server.logger.Warn(message, "error", text)
}

func redactTelegramError(err error, token string) error {
	if err == nil {
		return nil
	}
	return errors.New(redactTelegramText(err.Error(), token))
}

func redactTelegramText(value, token string) string {
	if strings.TrimSpace(token) != "" {
		value = strings.ReplaceAll(value, token, "[REDACTED]")
	}
	return telegramTokenInURLPattern.ReplaceAllString(value, "bot[REDACTED]")
}

func parseTelegramCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	commandToken, remainder, _ := strings.Cut(text, " ")
	commandToken = strings.TrimPrefix(commandToken, "/")
	if at := strings.IndexByte(commandToken, '@'); at >= 0 {
		commandToken = commandToken[:at]
	}
	return strings.ToLower(strings.TrimSpace(commandToken)), strings.TrimSpace(remainder)
}

func splitTelegramArguments(value string, count int) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 || count <= 0 {
		return nil
	}
	if len(fields) <= count {
		return fields
	}
	result := append([]string(nil), fields[:count-1]...)
	return append(result, strings.Join(fields[count-1:], " "))
}

func validTelegramDialNumber(number string) bool {
	number = strings.TrimSpace(number)
	if strings.HasPrefix(number, "+") {
		number = number[1:]
	}
	if len(number) < 3 || len(number) > 20 {
		return false
	}
	for _, character := range number {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func formatTelegramAT(response modem.Response) string {
	parts := make([]string, 0, 2)
	if text := strings.TrimSpace(response.Text()); text != "" {
		parts = append(parts, text)
	}
	if final := strings.TrimSpace(response.Final); final != "" {
		parts = append(parts, final)
	}
	if len(parts) == 0 {
		return "模块没有返回结果"
	}
	return strings.Join(parts, "\n")
}

func formatTelegramVoWiFiState(state vowifi.State) string {
	lines := []string{
		fmt.Sprintf("状态：%s", firstNonEmpty(string(state.Phase), "idle")),
		fmt.Sprintf("SIM=%t Access=%t Tunnel=%t IMS=%t SMS=%t", state.SIMReady, state.AccessReady, state.TunnelReady, state.IMSReady, state.SMSReady),
	}
	if state.LastReason != "" {
		lines = append(lines, "原因："+state.LastReason)
	}
	if state.LastError != "" {
		lines = append(lines, "错误："+state.LastError)
	}
	return strings.Join(lines, "\n")
}

func truncateTelegramText(value string, maximum int) string {
	runes := []rune(value)
	if maximum <= 0 || len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum]) + "…"
}

func tailDigits(value string, count int) string {
	value = strings.TrimSpace(value)
	if count <= 0 || len(value) <= count {
		return value
	}
	return value[len(value)-count:]
}

func waitTelegram(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
