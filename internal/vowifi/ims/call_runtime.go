package ims

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"vocat/internal/vowifi"
)

var (
	ErrCallNotFound = errors.New("ims: call not found")
	ErrCallState    = errors.New("ims: call is not in the required state")
)

type imsCall struct {
	public     vowifi.Call
	callID     string
	target     string
	from       string
	to         string
	branch     string
	cseq       uint32
	invite     *sipRequest
	respond    func([]byte) error
	responses  chan *sipResponse
	remoteTag  string
	routes     []string
	terminated bool
}

func (session *Session) Calls() []vowifi.Call {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	calls := make([]vowifi.Call, 0, len(session.calls))
	for _, call := range session.calls {
		if call.public.State != "ended" && call.public.State != "failed" {
			calls = append(calls, call.public)
		}
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].StartedAt.Before(calls[j].StartedAt) })
	return calls
}

func (session *Session) DialCall(ctx context.Context, number string) (vowifi.Call, error) {
	number = strings.TrimSpace(number)
	if !validCallNumber(number) {
		return vowifi.Call{}, errors.New("ims: invalid dial number")
	}
	callToken, err := randomHex(18)
	if err != nil {
		return vowifi.Call{}, err
	}
	branch, err := randomHex(12)
	if err != nil {
		return vowifi.Call{}, err
	}
	callID := callToken + "@" + addressHost(session.conn.LocalAddr())
	target := "tel:" + number
	session.mu.Lock()
	cseq := session.cseq
	session.cseq++
	routes := append([]string(nil), session.evidence.ServiceRoute...)
	securityHeaders := runtimeSecurityHeaders(session.securityActive, session.securityAgreement.verifyValue)
	session.mu.Unlock()
	body := session.inactiveSDP()
	transportUpper := strings.ToUpper(session.transport)
	from := "<" + session.identity.public + ">;tag=" + session.fromTag
	to := "<" + target + ">"
	lines := []string{
		"INVITE " + target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", transportUpper, session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	lines = append(lines, securityHeaders...)
	if len(routes) == 0 {
		lines = append(lines, "Route: <sip:"+session.endpoint.address()+";transport="+session.transport+";lr>")
	} else {
		for _, route := range routes {
			lines = append(lines, "Route: "+route)
		}
	}
	lines = append(lines,
		"From: "+from,
		"To: "+to,
		"Call-ID: "+callID,
		fmt.Sprintf("CSeq: %d INVITE", cseq),
		"Contact: <sip:"+session.identity.user+"@"+session.contactAddress()+";transport="+session.transport+">",
		"P-Preferred-Identity: <"+session.identity.public+">",
		"Allow: INVITE, ACK, CANCEL, BYE, OPTIONS, MESSAGE",
		"Supported: timer",
		"Content-Type: application/sdp",
		"Content-Length: "+strconv.Itoa(len(body)), "", "",
	)
	request := append([]byte(strings.Join(lines, "\r\n")), body...)
	responses := make(chan *sipResponse, 8)
	key := sipTransactionKey{callID: callID, cseq: cseq, method: "INVITE"}
	session.transactionsMu.Lock()
	if _, duplicate := session.transactions[key]; duplicate {
		session.transactionsMu.Unlock()
		return vowifi.Call{}, errors.New("ims: duplicate call transaction")
	}
	session.transactions[key] = responses
	session.transactionsMu.Unlock()
	call := &imsCall{
		public: vowifi.Call{ID: callID, Number: number, Direction: "outgoing", State: "dialing", StartedAt: time.Now().UTC()},
		callID: callID, target: target, from: from, to: to, branch: branch, cseq: cseq, responses: responses,
		routes: routes,
	}
	session.callMu.Lock()
	session.calls[callID] = call
	session.callMu.Unlock()
	session.writeMu.Lock()
	_, err = session.conn.Write(request)
	session.writeMu.Unlock()
	if err != nil {
		session.transactionsMu.Lock()
		delete(session.transactions, key)
		session.transactionsMu.Unlock()
		session.callMu.Lock()
		delete(session.calls, callID)
		session.callMu.Unlock()
		return vowifi.Call{}, fmt.Errorf("ims: send SIP INVITE: %w", err)
	}
	go session.watchOutgoingCall(call, key)
	return call.public, nil
}

func (session *Session) watchOutgoingCall(call *imsCall, key sipTransactionKey) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	defer func() {
		session.transactionsMu.Lock()
		delete(session.transactions, key)
		session.transactionsMu.Unlock()
	}()
	for {
		select {
		case <-session.refreshContext.Done():
			return
		case <-timer.C:
			session.setCallState(call.callID, "failed")
			return
		case response := <-call.responses:
			if response == nil {
				continue
			}
			if response.StatusCode < 200 {
				if response.StatusCode >= 180 {
					session.setCallState(call.callID, "ringing")
				}
				continue
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				session.callMu.Lock()
				call.to = response.value("To")
				call.remoteTag = headerParameter(call.to, "tag")
				if contact := headerURI(response.value("Contact")); contact != "" {
					call.target = contact
				}
				if recordRoutes := response.values("Record-Route"); len(recordRoutes) > 0 {
					call.routes = reverseStrings(recordRoutes)
				}
				session.callMu.Unlock()
				_ = session.sendACK(call)
				session.setCallState(call.callID, "active")
			} else {
				session.setCallState(call.callID, "failed")
			}
			return
		}
	}
}

func (session *Session) AnswerCall(_ context.Context, id string) (vowifi.Call, error) {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallNotFound
	}
	if call.public.Direction != "incoming" || call.public.State != "ringing" || call.invite == nil || call.respond == nil {
		session.callMu.Unlock()
		return vowifi.Call{}, ErrCallState
	}
	request, respond := call.invite, call.respond
	session.callMu.Unlock()
	response, err := buildSIPResponseWithBody(request, 200, session.fromTag, session.inactiveSDP())
	if err != nil {
		return vowifi.Call{}, err
	}
	if err := respond(response); err != nil {
		return vowifi.Call{}, err
	}
	session.setCallState(id, "active")
	session.callMu.Lock()
	result := call.public
	session.callMu.Unlock()
	return result, nil
}

func (session *Session) HangupCall(ctx context.Context, id string) error {
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return ErrCallNotFound
	}
	state := call.public.State
	direction := call.public.Direction
	request, respond := call.invite, call.respond
	session.callMu.Unlock()
	if direction == "incoming" && state == "ringing" && request != nil && respond != nil {
		response, err := buildSIPResponseWithBody(request, 486, session.fromTag, nil)
		if err != nil {
			return err
		}
		if err := respond(response); err != nil {
			return err
		}
		session.setCallState(id, "ended")
		return nil
	}
	method := "BYE"
	if direction == "outgoing" && (state == "dialing" || state == "ringing") {
		method = "CANCEL"
	}
	if err := session.sendDialogRequest(ctx, call, method); err != nil {
		return err
	}
	session.setCallState(id, "ended")
	return nil
}

func (session *Session) handleCallRequest(request *sipRequest, respond func([]byte) error) bool {
	switch request.Method {
	case "INVITE":
		callID := strings.TrimSpace(request.value("Call-ID"))
		if callID == "" {
			return true
		}
		number := identityNumber(request.value("From"))
		target := headerURI(request.value("Contact"))
		if target == "" {
			target = request.URI
		}
		call := &imsCall{
			public: vowifi.Call{ID: callID, Number: number, Direction: "incoming", State: "ringing", StartedAt: time.Now().UTC()},
			callID: callID, target: target, from: request.value("To") + ";tag=" + session.fromTag,
			to: request.value("From"), invite: request, respond: respond, routes: request.values("Record-Route"),
		}
		session.callMu.Lock()
		session.calls[callID] = call
		session.callMu.Unlock()
		response, err := buildSIPResponseWithBody(request, 180, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		return true
	case "ACK":
		return true
	case "CANCEL", "BYE":
		response, err := buildSIPResponseWithBody(request, 200, session.fromTag, nil)
		if err == nil {
			_ = respond(response)
		}
		callID := strings.TrimSpace(request.value("Call-ID"))
		if request.Method == "CANCEL" {
			session.callMu.Lock()
			call := session.calls[callID]
			session.callMu.Unlock()
			if call != nil && call.invite != nil && call.respond != nil {
				if terminated, buildErr := buildSIPResponseWithBody(call.invite, 487, session.fromTag, nil); buildErr == nil {
					_ = call.respond(terminated)
				}
			}
		}
		session.setCallState(callID, "ended")
		return true
	default:
		return false
	}
}

func (session *Session) sendACK(call *imsCall) error {
	request := session.buildDialogRequest(call, "ACK", call.cseq)
	session.writeMu.Lock()
	_, err := session.conn.Write(request)
	session.writeMu.Unlock()
	return err
}

func (session *Session) sendDialogRequest(ctx context.Context, call *imsCall, method string) error {
	cseq := call.cseq
	if method == "BYE" {
		session.mu.Lock()
		cseq = session.cseq
		session.cseq++
		session.mu.Unlock()
	}
	request := session.buildDialogRequest(call, method, cseq)
	if method == "ACK" {
		session.writeMu.Lock()
		_, err := session.conn.Write(request)
		session.writeMu.Unlock()
		return err
	}
	response, err := session.exchangeRuntime(ctx, request, sipTransactionKey{callID: call.callID, cseq: cseq, method: method})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ims: SIP %s rejected with %d", method, response.StatusCode)
	}
	return nil
}

func (session *Session) buildDialogRequest(call *imsCall, method string, cseq uint32) []byte {
	branch, _ := randomHex(12)
	if method == "CANCEL" {
		branch = call.branch
	}
	to := call.to
	if to == "" {
		to = "<" + call.target + ">"
	}
	lines := []string{
		method + " " + call.target + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/%s %s;branch=z9hG4bK%s;rport", strings.ToUpper(session.transport), session.conn.LocalAddr().String(), branch),
		"Max-Forwards: 70",
	}
	for _, route := range call.routes {
		lines = append(lines, "Route: "+route)
	}
	lines = append(lines,
		"From: "+call.from,
		"To: "+to,
		"Call-ID: "+call.callID,
		fmt.Sprintf("CSeq: %d %s", cseq, method),
		"Content-Length: 0", "", "",
	)
	return []byte(strings.Join(lines, "\r\n"))
}

func (session *Session) inactiveSDP() []byte {
	var localAddress net.Addr
	if session.conn != nil {
		localAddress = session.conn.LocalAddr()
	}
	local := addressIP(localAddress)
	if local == nil {
		local = net.IPv4zero
	}
	family := "IP4"
	if local.To4() == nil {
		family = "IP6"
	}
	text := fmt.Sprintf("v=0\r\no=- %d %d IN %s %s\r\ns=VoCat Calling Test\r\nc=IN %s %s\r\nt=0 0\r\nm=audio 9 RTP/AVP 0 8\r\na=inactive\r\n", time.Now().Unix(), time.Now().Unix(), family, local.String(), family, local.String())
	return []byte(text)
}

func buildSIPResponseWithBody(request *sipRequest, status int, tag string, body []byte) ([]byte, error) {
	reasons := map[int]string{180: "Ringing", 200: "OK", 486: "Busy Here", 487: "Request Terminated"}
	reason := reasons[status]
	if reason == "" {
		return nil, errors.New("ims: unsupported call response status")
	}
	via := request.values("Via")
	from, to := request.value("From"), request.value("To")
	callID, cseq := request.value("Call-ID"), request.value("CSeq")
	if len(via) == 0 || from == "" || to == "" || callID == "" || cseq == "" {
		return nil, errors.New("ims: call request omitted a mandatory response header")
	}
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + tag
	}
	lines := []string{fmt.Sprintf("SIP/2.0 %d %s", status, reason)}
	for _, value := range via {
		lines = append(lines, "Via: "+value)
	}
	lines = append(lines, "From: "+from, "To: "+to, "Call-ID: "+callID, "CSeq: "+cseq)
	if len(body) > 0 {
		lines = append(lines, "Content-Type: application/sdp")
	}
	lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)), "", "")
	return append([]byte(strings.Join(lines, "\r\n")), body...), nil
}

func (session *Session) setCallState(id, state string) {
	session.callMu.Lock()
	if call := session.calls[id]; call != nil {
		call.public.State = state
	}
	session.callMu.Unlock()
}

func validCallNumber(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if character >= '0' && character <= '9' || index == 0 && character == '+' || character == '*' || character == '#' {
			continue
		}
		return false
	}
	return true
}

func identityNumber(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			value = value[start+1 : start+end]
		}
	}
	value = strings.TrimPrefix(value, "sip:")
	value = strings.TrimPrefix(value, "tel:")
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[:at]
	}
	return strings.TrimSpace(value)
}

func headerParameter(value, name string) string {
	needle := ";" + strings.ToLower(name) + "="
	lower := strings.ToLower(value)
	index := strings.Index(lower, needle)
	if index < 0 {
		return ""
	}
	value = value[index+len(needle):]
	if end := strings.IndexAny(value, ";,> \t"); end >= 0 {
		value = value[:end]
	}
	return strings.Trim(value, `"`)
}

func headerURI(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.Index(value, "<"); start >= 0 {
		if end := strings.Index(value[start+1:], ">"); end >= 0 {
			return strings.TrimSpace(value[start+1 : start+1+end])
		}
	}
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	if strings.HasPrefix(strings.ToLower(value), "sip:") || strings.HasPrefix(strings.ToLower(value), "tel:") {
		return strings.TrimSpace(value)
	}
	return ""
}

func reverseStrings(values []string) []string {
	result := append([]string(nil), values...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

var _ vowifi.CallController = (*Session)(nil)
