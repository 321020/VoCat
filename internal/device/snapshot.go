package device

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"vocat/internal/modem"
)

func (manager *Manager) readSnapshot(
	ctx context.Context,
	id string,
	candidate modem.Candidate,
	backend string,
	previousICCID string,
	client modem.Client,
) (Snapshot, error) {
	snapshot := Snapshot{
		DeviceID:      id,
		Port:          candidate.ATPort.OpenPath(),
		OperatingMode: -1,
		UpdatedAt:     time.Now().UTC(),
	}
	ati, err := manager.command(ctx, client, "ATI")
	if err != nil {
		return snapshot, fmt.Errorf("probe modem: %w", err)
	}
	snapshot.Responsive = true
	snapshot.Manufacturer, snapshot.Model, snapshot.Firmware = parseATI(ati.Lines)
	if snapshot.Model == "" && !strings.EqualFold(candidate.Product, "Android") {
		snapshot.Model = candidate.Product
	}

	optional := func(command string) (modem.Response, bool) {
		response, commandErr := manager.command(ctx, client, command)
		if commandErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, commandErr.Error())
			return response, false
		}
		return response, true
	}

	if response, ok := optional("AT+CPIN?"); ok {
		snapshot.SIMStatus, snapshot.SIMReady = parseCPIN(response)
	}
	ccid, ccidErr := manager.command(ctx, client, "AT+CCID")
	if ccidErr != nil && candidate.HardwareKind == "sierra_usb" {
		ccid, ccidErr = manager.command(ctx, client, "AT!ICCID?")
	}
	if ccidErr != nil {
		ccid, ccidErr = manager.command(ctx, client, "AT+QCCID")
	}
	if ccidErr != nil {
		snapshot.Warnings = append(snapshot.Warnings, "read ICCID: "+ccidErr.Error())
	} else {
		snapshot.ICCID = parseICCIDIdentifier(ccid, []string{"+CCID:", "+QCCID:", "!ICCID:"}, 18, 22)
	}
	previousICCID = strings.TrimSpace(previousICCID)
	if previousICCID != "" && snapshot.ICCID != "" && !strings.EqualFold(previousICCID, snapshot.ICCID) {
		// A different physical SIM must never inherit the previous card's
		// permission to use cellular RF. Disable RF before reading serving-cell
		// or operator state; policy reconciliation will then start VoWiFi.
		if _, err := manager.command(ctx, client, "AT+CFUN=4"); err != nil {
			return snapshot, fmt.Errorf("protect changed SIM with RF off: %w", err)
		}
		snapshot.SIMChanged = true
	}
	if response, ok := optional("AT+CIMI"); ok {
		snapshot.IMSI = parseIdentifier(response, []string{"+CIMI:"}, 10, 18)
	}
	// EF_SPN is the SIM-issued brand (for example "Lebara"), which is distinct
	// from the IMSI sponsor/core PLMN. A Lebara UK subscription may therefore
	// legitimately carry a Vodafone NL IMSI while still presenting Lebara as
	// its customer-facing operator. Failure is intentionally silent because
	// EF_SPN is optional and some physical SIMs deny CRSM access to it.
	if response, spnErr := manager.command(ctx, client, "AT+CRSM=176,28486,0,0,17"); spnErr == nil {
		snapshot.SPN = parseSPN(response)
	}
	if response, ok := optional("AT+CSQ"); ok {
		snapshot.SignalRaw, snapshot.SignalPercent, snapshot.RSSIDBm = parseCSQ(response)
	}
	servingPLMN := ""
	servingCommand := `AT+QENG="servingcell"`
	if candidate.HardwareKind == "sierra_usb" {
		servingCommand = "AT!GSTATUS?"
	}
	if response, ok := optional(servingCommand); ok {
		metrics := parseQENG(response)
		if candidate.HardwareKind == "sierra_usb" {
			metrics = parseSierraGStatus(response)
		}
		servingPLMN = metrics.PLMN
		snapshot.AccessTech = metrics.AccessTech
		snapshot.Band = metrics.Band
		snapshot.Channel = metrics.Channel
		snapshot.RSRP = metrics.RSRP
		snapshot.RSRQ = metrics.RSRQ
		snapshot.SINR = metrics.SINR
		if metrics.RSSI != nil {
			snapshot.RSSIDBm = metrics.RSSI
		}
	}
	if response, ok := optional("AT+COPS?"); ok {
		operator := parseCOPS(response)
		if operator.Code != "" {
			snapshot.OperatorCode = operator.Code
		} else {
			snapshot.OperatorCode = servingPLMN
		}
		snapshot.OperatorName = carrierNameForPLMN(snapshot.OperatorCode, operator.Name)
		if snapshot.AccessTech == "" {
			snapshot.AccessTech = operator.AccessTech
		}
	}
	for _, command := range []string{"AT+CEREG?", "AT+CGREG?", "AT+CREG?"} {
		response, registrationErr := manager.command(ctx, client, command)
		if registrationErr != nil {
			continue
		}
		if status, found := parseRegistrationStatus(response); found {
			snapshot.RegistrationStatus = status
			snapshot.RegistrationSource = strings.TrimSuffix(strings.TrimPrefix(command, "AT+"), "?")
			break
		}
	}
	if strings.EqualFold(backend, "qmi") {
		registration, found := readPlatformRegistration(ctx, candidate)
		if found {
			snapshot.RegistrationStatus = registration.Status
			snapshot.RegistrationSource = "QMI NAS"
			snapshot.PSAttached = registration.PSAttached
			if registration.PLMN != "" {
				snapshot.OperatorCode = registration.PLMN
				snapshot.OperatorName = carrierNameForPLMN(registration.PLMN, registration.Name)
			}
		}
	}
	if snapshot.RegistrationSource == "" && (snapshot.OperatorName != "" || snapshot.OperatorCode != "") {
		// Older firmware can omit registration queries while COPS still proves
		// that an operator is selected.
		snapshot.RegistrationStatus = 1
		snapshot.RegistrationSource = "COPS"
	}
	if response, ok := optional("AT+CGSN"); ok {
		snapshot.IMEI = parseIdentifier(
			response,
			[]string{"+CGSN:", "+GSN:"},
			14,
			17,
		)
	}

	if response, ok := optional("AT+CFUN?"); ok {
		if mode, found := parseCFUN(response); found {
			snapshot.OperatingMode = mode
			snapshot.ModeKnown = true
			snapshot.FlightMode = isRadioOffMode(mode)
			snapshot.RadioOff = snapshot.FlightMode
		}
	}

	phone, warnings := manager.readPhoneNumber(ctx, client)
	snapshot.Phone = phone
	snapshot.Warnings = append(snapshot.Warnings, warnings...)
	snapshot.UpdatedAt = time.Now().UTC()
	return snapshot, nil
}

func parseSPN(response modem.Response) string {
	value := valueAfterPrefix(response, "+CRSM:")
	fields := csvValues(value)
	if len(fields) < 3 {
		return ""
	}
	sw1, sw1Err := strconv.Atoi(strings.TrimSpace(fields[0]))
	sw2, sw2Err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if sw1Err != nil || sw2Err != nil || (sw1 != 0x90 && sw1 != 0x91 && sw1 != 0x9f) || sw2 < 0 || sw2 > 255 {
		return ""
	}
	raw, err := hex.DecodeString(strings.Trim(strings.TrimSpace(fields[2]), `"`))
	if err != nil || len(raw) < 2 {
		return ""
	}
	alpha := raw[1:] // byte 0 is the display-condition bit field.
	for len(alpha) > 0 && (alpha[len(alpha)-1] == 0xff || alpha[len(alpha)-1] == 0x00) {
		alpha = alpha[:len(alpha)-1]
	}
	if len(alpha) == 0 {
		return ""
	}
	if alpha[0] == 0x80 {
		ucs2 := alpha[1:]
		if len(ucs2)%2 != 0 {
			ucs2 = ucs2[:len(ucs2)-1]
		}
		units := make([]uint16, 0, len(ucs2)/2)
		for index := 0; index+1 < len(ucs2); index += 2 {
			unit := uint16(ucs2[index])<<8 | uint16(ucs2[index+1])
			if unit != 0xffff && unit != 0 {
				units = append(units, unit)
			}
		}
		return strings.TrimSpace(string(utf16.Decode(units)))
	}
	// EF_SPN uses the unpacked GSM default alphabet. Its printable Latin subset
	// is byte-compatible with UTF-8/ASCII and covers operator brands in practice.
	printable := make([]byte, 0, len(alpha))
	for _, value := range alpha {
		if value >= 0x20 && value <= 0x7e {
			printable = append(printable, value)
		}
	}
	return strings.TrimSpace(string(printable))
}

func parseRegistrationStatus(response modem.Response) (int, bool) {
	for _, prefix := range []string{"+CEREG:", "+CGREG:", "+CREG:"} {
		values := csvValues(valueAfterPrefix(response, prefix))
		if len(values) == 0 {
			continue
		}
		index := 0
		// Query responses are <n>,<stat>; unsolicited responses are <stat>.
		if len(values) >= 2 {
			index = 1
		}
		status, err := strconv.Atoi(strings.TrimSpace(values[index]))
		if err == nil && status >= 0 && status <= 10 {
			return status, true
		}
	}
	return 0, false
}

func parseATI(lines []string) (manufacturer, model, firmware string) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "REVISION:"):
			firmware = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		case strings.Contains(upper, "QUECTEL"):
			manufacturer = line
		case strings.HasPrefix(upper, "EC20") || strings.HasPrefix(upper, "EC25"):
			model = line
		}
	}
	return
}

func parseCPIN(response modem.Response) (string, bool) {
	value := strings.ToUpper(valueAfterPrefix(response, "+CPIN:"))
	switch {
	case strings.Contains(value, "READY"):
		return "ready", true
	case strings.Contains(value, "SIM PIN"):
		return "pin_required", false
	case strings.Contains(value, "SIM PUK"):
		return "puk_required", false
	case strings.Contains(value, "NOT INSERTED"):
		return "not_inserted", false
	case value == "":
		return "unknown", false
	default:
		return strings.ToLower(strings.ReplaceAll(value, " ", "_")), false
	}
}

func parseCSQ(response modem.Response) (raw, percent, dbm *int) {
	values := csvValues(valueAfterPrefix(response, "+CSQ:"))
	if len(values) == 0 {
		return nil, nil, nil
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < 0 || value > 31 {
		return nil, nil, nil
	}
	raw = intPointer(value)
	scaled := (value*100 + 15) / 31
	percent = intPointer(scaled)
	signalDBM := -113 + value*2
	dbm = intPointer(signalDBM)
	return
}

type qengMetrics struct {
	PLMN       string
	AccessTech string
	Band       string
	Channel    string
	RSSI       *int
	RSRP       *int
	RSRQ       *int
	SINR       *int
}

func parseQENG(response modem.Response) qengMetrics {
	for _, line := range response.Lines {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "+QENG:") {
			continue
		}
		values := csvValues(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		if len(values) < 3 || !strings.EqualFold(values[0], "servingcell") {
			continue
		}
		result := qengMetrics{AccessTech: strings.ToUpper(values[2])}
		if strings.EqualFold(values[2], "LTE") && len(values) >= 17 {
			if decimalDigits(values[4], 3, 3) && decimalDigits(values[5], 2, 3) {
				result.PLMN = values[4] + values[5]
			}
			result.Channel = values[8]
			if values[9] != "" {
				result.Band = "B" + values[9]
			}
			result.RSRP = parseOptionalInt(values[13])
			result.RSRQ = parseOptionalInt(values[14])
			result.RSSI = parseOptionalInt(values[15])
			result.SINR = parseOptionalInt(values[16])
		}
		return result
	}
	return qengMetrics{}
}

var sierraGStatusFields = map[string]*regexp.Regexp{
	"mode":    regexp.MustCompile(`(?i)System mode:\s*([^\s]+)`),
	"band":    regexp.MustCompile(`(?i)LTE band:\s*([^\s]+)`),
	"channel": regexp.MustCompile(`(?i)LTE Rx chan:\s*([0-9]+)`),
	"rssi":    regexp.MustCompile(`(?i)RSSI \(dBm\):\s*(-?[0-9]+(?:\.[0-9]+)?)`),
	"rsrp":    regexp.MustCompile(`(?i)RSRP \(dBm\):\s*(-?[0-9]+(?:\.[0-9]+)?)`),
	"rsrq":    regexp.MustCompile(`(?i)RSRQ \(dB\):\s*(-?[0-9]+(?:\.[0-9]+)?)`),
	"sinr":    regexp.MustCompile(`(?i)SINR \(dB\):\s*(-?[0-9]+(?:\.[0-9]+)?)`),
}

func parseSierraGStatus(response modem.Response) qengMetrics {
	text := strings.Join(response.Lines, "\n")
	result := qengMetrics{}
	if match := sierraGStatusFields["mode"].FindStringSubmatch(text); len(match) == 2 {
		result.AccessTech = strings.ToUpper(strings.TrimSpace(match[1]))
	}
	if match := sierraGStatusFields["band"].FindStringSubmatch(text); len(match) == 2 {
		result.Band = strings.ToUpper(strings.TrimSpace(match[1]))
	}
	if match := sierraGStatusFields["channel"].FindStringSubmatch(text); len(match) == 2 {
		result.Channel = match[1]
	}
	result.RSSI = parseSierraDecimalMetric(text, "rssi")
	result.RSRP = parseSierraDecimalMetric(text, "rsrp")
	result.RSRQ = parseSierraDecimalMetric(text, "rsrq")
	result.SINR = parseSierraDecimalMetric(text, "sinr")
	return result
}

func parseSierraDecimalMetric(text, field string) *int {
	match := sierraGStatusFields[field].FindStringSubmatch(text)
	if len(match) != 2 {
		return nil
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	result := int(math.Round(value))
	return &result
}

func decimalDigits(value string, minimum, maximum int) bool {
	value = strings.TrimSpace(value)
	return len(value) >= minimum && len(value) <= maximum && strings.IndexFunc(value, func(character rune) bool {
		return character < '0' || character > '9'
	}) < 0
}

type operatorInfo struct {
	Name       string
	Code       string
	AccessTech string
}

func parseCOPS(response modem.Response) operatorInfo {
	values := csvValues(valueAfterPrefix(response, "+COPS:"))
	if len(values) < 3 {
		return operatorInfo{}
	}
	result := operatorInfo{Name: values[2]}
	format, _ := strconv.Atoi(values[1])
	if format == 2 {
		result.Code = values[2]
		result.Name = ""
	}
	if len(values) >= 4 {
		result.AccessTech = accessTechnology(values[3])
	}
	return result
}

func accessTechnology(value string) string {
	switch strings.TrimSpace(value) {
	case "0":
		return "GSM"
	case "2":
		return "UTRAN"
	case "3":
		return "EDGE"
	case "4":
		return "HSDPA"
	case "5":
		return "HSUPA"
	case "6":
		return "HSPA"
	case "7":
		return "LTE"
	case "9":
		return "NR5G"
	default:
		return ""
	}
}

func parseCFUN(response modem.Response) (int, bool) {
	values := csvValues(valueAfterPrefix(response, "+CFUN:"))
	if len(values) == 0 {
		return 0, false
	}
	mode, err := strconv.Atoi(values[0])
	return mode, err == nil
}

func isRadioOffMode(mode int) bool {
	return mode == 0 || mode == 4
}

func valueAfterPrefix(response modem.Response, prefix string) string {
	for _, line := range response.Lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(prefix)) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func csvValues(value string) []string {
	reader := csv.NewReader(strings.NewReader(value))
	reader.TrimLeadingSpace = true
	reader.LazyQuotes = true
	record, err := reader.Read()
	if err != nil && err != io.EOF {
		return nil
	}
	for index := range record {
		record[index] = strings.TrimSpace(record[index])
	}
	return record
}

func firstDigitLine(response modem.Response, minimum, maximum int) string {
	for _, line := range response.Lines {
		value := strings.TrimSpace(line)
		if len(value) < minimum || len(value) > maximum {
			continue
		}
		if strings.IndexFunc(value, func(character rune) bool {
			return !unicode.IsDigit(character)
		}) < 0 {
			return value
		}
	}
	return ""
}

func parseIdentifier(
	response modem.Response,
	prefixes []string,
	minimum, maximum int,
) string {
	for _, prefix := range prefixes {
		value := strings.Trim(valueAfterPrefix(response, prefix), `" `)
		if len(value) >= minimum && len(value) <= maximum &&
			strings.IndexFunc(value, func(character rune) bool {
				return !unicode.IsDigit(character)
			}) < 0 {
			return value
		}
	}
	return firstDigitLine(response, minimum, maximum)
}

// parseICCIDIdentifier accepts all trailing hexadecimal F nibbles exposed from
// the fixed 10-octet EF-ICCID representation. A 19-digit ICCID has one filler
// nibble while an 18-digit ICCID has two; neither is part of the identifier.
func parseICCIDIdentifier(
	response modem.Response,
	prefixes []string,
	minimum, maximum int,
) string {
	normalize := func(value string) string {
		value = strings.Trim(value, `" `)
		value = strings.TrimRight(value, "Ff")
		if len(value) >= minimum && len(value) <= maximum &&
			strings.IndexFunc(value, func(character rune) bool { return !unicode.IsDigit(character) }) < 0 {
			return value
		}
		return ""
	}
	for _, prefix := range prefixes {
		if value := normalize(valueAfterPrefix(response, prefix)); value != "" {
			return value
		}
	}
	for _, line := range response.Lines {
		if value := normalize(strings.TrimSpace(line)); value != "" {
			return value
		}
	}
	return ""
}

func parseOptionalInt(value string) *int {
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return intPointer(number)
}

func intPointer(value int) *int {
	return &value
}
