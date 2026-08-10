package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"vocat/internal/device"
	"vocat/internal/exportproxy"
	"vocat/internal/store"
)

const (
	automaticTaskPollInterval = 5 * time.Second
	automaticTaskMaxRuntime   = 8 * time.Minute
)

type automaticTaskPayload struct {
	Phone           string `json:"phone,omitempty"`
	Message         string `json:"message,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
}

type automaticTaskExecutionError struct {
	err       error
	retryable bool
}

func (value automaticTaskExecutionError) Error() string { return value.err.Error() }
func (value automaticTaskExecutionError) Unwrap() error { return value.err }

type automaticTaskScheduler struct {
	server *Server
	ctx    context.Context
	mu     sync.Mutex
	queues map[string]chan store.AutomaticTaskRun
}

func (s *Server) StartAutomaticTasks(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	scheduler := &automaticTaskScheduler{server: s, ctx: ctx, queues: make(map[string]chan store.AutomaticTaskRun)}
	s.automaticTasks = scheduler
	go scheduler.run()
}

func (scheduler *automaticTaskScheduler) run() {
	ticker := time.NewTicker(automaticTaskPollInterval)
	defer ticker.Stop()
	scheduler.claim()
	for {
		select {
		case <-scheduler.ctx.Done():
			return
		case <-ticker.C:
			scheduler.claim()
		}
	}
}

func (scheduler *automaticTaskScheduler) claim() {
	runs, err := scheduler.server.store.ClaimDueAutomaticTasks(scheduler.ctx, time.Now().UTC(), 50)
	if err != nil {
		scheduler.server.logger.Warn("claim automatic tasks", "error", err)
		return
	}
	for _, run := range runs {
		scheduler.enqueue(run)
	}
}

func (scheduler *automaticTaskScheduler) enqueue(run store.AutomaticTaskRun) {
	deviceID := strings.TrimSpace(run.DeviceID)
	scheduler.mu.Lock()
	queue := scheduler.queues[deviceID]
	if queue == nil {
		queue = make(chan store.AutomaticTaskRun, 100)
		scheduler.queues[deviceID] = queue
		go scheduler.worker(deviceID, queue)
	}
	scheduler.mu.Unlock()
	select {
	case queue <- run:
	case <-scheduler.ctx.Done():
	}
}

func (scheduler *automaticTaskScheduler) worker(deviceID string, queue <-chan store.AutomaticTaskRun) {
	for {
		select {
		case <-scheduler.ctx.Done():
			return
		case run := <-queue:
			scheduler.execute(run)
		}
	}
}

func (scheduler *automaticTaskScheduler) execute(run store.AutomaticTaskRun) {
	task, err := scheduler.server.store.AutomaticTask(scheduler.ctx, run.TaskID)
	if err != nil {
		run.Status, run.Error, run.FinishedAt = "failed", err.Error(), time.Now().UTC()
		_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		return
	}
	run.Status, run.StartedAt = "running", time.Now().UTC()
	_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
	var output string
	for attempt := 1; attempt <= task.RetryCount+1; attempt++ {
		run.Attempts = attempt
		_ = scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run)
		operationContext, cancel := context.WithTimeout(scheduler.ctx, automaticTaskMaxRuntime)
		output, err = scheduler.server.executeAutomaticTask(operationContext, task)
		cancel()
		if err == nil {
			break
		}
		var executionError automaticTaskExecutionError
		if errors.As(err, &executionError) && !executionError.retryable {
			break
		}
		if attempt <= task.RetryCount {
			scheduler.server.logger.Warn("automatic task attempt failed", "task_id", task.ID, "device_id", task.DeviceID, "attempt", attempt, "error", err)
			select {
			case <-scheduler.ctx.Done():
				break
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
		}
	}
	run.FinishedAt = time.Now().UTC()
	if err == nil {
		run.Status, run.Output, run.Error = "success", output, ""
	} else {
		run.Status, run.Error = "failed", err.Error()
	}
	if updateErr := scheduler.server.store.UpdateAutomaticTaskRun(context.Background(), run); updateErr != nil {
		scheduler.server.logger.Warn("finish automatic task run", "run_id", run.ID, "error", updateErr)
	}
	if task.Notify {
		go scheduler.server.notifyAutomaticTask(context.Background(), task, run)
	}
}

func (s *Server) executeAutomaticTask(ctx context.Context, task store.AutomaticTask) (string, error) {
	config, entry, physicalID, err := s.ensureAutomaticTaskProfile(ctx, task)
	if err != nil {
		return "", err
	}
	networkWasEnabled := config.NetworkEnabled
	if err := s.prepareAutomaticTaskEnvironment(ctx, &config, entry, physicalID, task); err != nil {
		return "", err
	}
	var payload automaticTaskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode task payload: %w", err)
	}
	switch task.TaskType {
	case "sms":
		return s.executeAutomaticSMS(ctx, task, payload)
	case "call":
		return s.executeAutomaticCall(ctx, task, payload)
	case "public_ip":
		return s.executeAutomaticPublicIP(ctx, config, physicalID, task.ProfileICCID, networkWasEnabled)
	default:
		return "", fmt.Errorf("unsupported automatic task type %q", task.TaskType)
	}
}

func (s *Server) ensureAutomaticTaskProfile(ctx context.Context, task store.AutomaticTask) (store.Device, device.Device, string, error) {
	config, err := s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return store.Device{}, device.Device{}, "", fmt.Errorf("read device: %w", err)
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !present || entry.Snapshot == nil {
		return store.Device{}, device.Device{}, "", errors.New("configured device is offline")
	}
	if strings.EqualFold(strings.TrimSpace(entry.Snapshot.ICCID), strings.TrimSpace(task.ProfileICCID)) {
		return config, entry, physicalID, nil
	}
	if _, err := s.devices.SetFlight(ctx, physicalID, true); err != nil {
		return store.Device{}, device.Device{}, "", fmt.Errorf("enter airplane mode before profile switch: %w", err)
	}
	if err := s.devices.ESIMSwitchProfile(ctx, physicalID, task.ProfileICCID, task.ProfileAID); err != nil {
		return store.Device{}, device.Device{}, "", fmt.Errorf("switch eSIM profile: %w", err)
	}
	entry, physicalID, present = s.physicalForConfig(config)
	if !present {
		return store.Device{}, device.Device{}, "", errors.New("device did not recover after profile switch")
	}
	snapshot, err := s.devices.Refresh(ctx, physicalID)
	if err != nil {
		return store.Device{}, device.Device{}, "", fmt.Errorf("verify switched profile: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(snapshot.ICCID), strings.TrimSpace(task.ProfileICCID)) {
		return store.Device{}, device.Device{}, "", fmt.Errorf("profile verification failed: current ICCID is %s", firstNonEmpty(snapshot.ICCID, "unavailable"))
	}
	entry.Snapshot = &snapshot
	return config, entry, physicalID, nil
}

func (s *Server) prepareAutomaticTaskEnvironment(ctx context.Context, config *store.Device, entry device.Device, physicalID string, task store.AutomaticTask) error {
	iccid := strings.TrimSpace(task.ProfileICCID)
	if task.Environment == "vowifi" {
		if task.TaskType == "public_ip" {
			return errors.New("public IP tasks cannot run over VoWiFi")
		}
		if _, err := s.devices.SetFlight(ctx, physicalID, true); err != nil {
			return fmt.Errorf("enable airplane mode for VoWiFi: %w", err)
		}
		config.VoWiFiEnabled, config.NetworkEnabled = true, false
		if err := s.store.UpsertDevice(ctx, *config); err != nil {
			return err
		}
		if err := s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: iccid, VoWiFiEnabled: true, AirplaneEnabled: true, IPVersion: "IPV4V6", Source: "automatic_task"}); err != nil {
			return err
		}
		if s.vowifi == nil {
			return errors.New("VoWiFi runtime is unavailable")
		}
		state, stateErr := s.vowifi.State(config.ID)
		stateMatchesCard := state.ICCID == "" || strings.EqualFold(strings.TrimSpace(state.ICCID), iccid)
		if stateErr == nil && stateMatchesCard && state.IMSReady && (task.TaskType != "sms" || state.SMSReady) {
			return nil
		}
		if stateErr == nil && state.Enabled {
			_, stateErr = s.vowifi.RequestReconnect(config.ID)
		} else {
			_, stateErr = s.vowifi.RequestEnabled(config.ID, true)
		}
		if stateErr != nil {
			return fmt.Errorf("start VoWiFi: %w", stateErr)
		}
		return s.waitAutomaticVoWiFi(ctx, config.ID, iccid, task.TaskType == "sms")
	}
	if s.vowifi != nil {
		if state, stateErr := s.vowifi.State(config.ID); stateErr == nil && (state.Enabled || state.Active) {
			if _, stateErr = s.vowifi.RequestEnabled(config.ID, false); stateErr != nil {
				return fmt.Errorf("stop VoWiFi: %w", stateErr)
			}
			if err := s.waitAutomaticVoWiFiStopped(ctx, config.ID); err != nil {
				return err
			}
		}
	}
	config.VoWiFiEnabled = false
	config.NetworkEnabled = task.TaskType == "public_ip"
	if err := s.store.UpsertDevice(ctx, *config); err != nil {
		return err
	}
	if err := s.store.UpsertCardPolicy(ctx, store.CardPolicy{ICCID: iccid, NetworkEnabled: config.NetworkEnabled, VoWiFiEnabled: false, AirplaneEnabled: false, APN: config.APN, IPVersion: "IPV4V6", Source: "automatic_task"}); err != nil {
		return err
	}
	if task.TaskType != "public_ip" {
		if _, err := s.devices.SetNetwork(ctx, physicalID, device.NetworkRequest{Enabled: false, APN: config.APN, IPVersion: "IPV4V6", Backend: config.DeviceBackend}); err != nil {
			s.logger.Warn("automatic task could not stop unused cellular data", "device_id", config.ID, "error", err)
		}
	}
	if _, err := s.devices.SetFlight(ctx, physicalID, false); err != nil {
		return fmt.Errorf("enable cellular radio: %w", err)
	}
	if _, err := s.devices.SetOperatorSelection(ctx, physicalID, true, "", nil); err != nil {
		return fmt.Errorf("enable automatic network selection: %w", err)
	}
	if _, err := s.devices.ReRegisterOperator(ctx, physicalID); err != nil {
		return fmt.Errorf("re-register cellular network: %w", err)
	}
	if err := s.waitAutomaticCellular(ctx, physicalID, task.TaskType == "public_ip"); err != nil {
		return err
	}
	if task.TaskType == "public_ip" {
		if !s.developerActive(ctx) {
			return errors.New("roaming public IP tasks require developer mode")
		}
		if _, err := s.devices.SetNetwork(ctx, physicalID, device.NetworkRequest{Enabled: true, APN: config.APN, IPVersion: "IPV4V6", Backend: config.DeviceBackend}); err != nil {
			s.rollbackAutomaticNetwork(config.ID, physicalID, iccid, *config)
			return fmt.Errorf("start roaming data: %w", err)
		}
	}
	return nil
}

func (s *Server) waitAutomaticVoWiFi(ctx context.Context, deviceID, iccid string, requireSMS bool) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		state, err := s.vowifi.State(deviceID)
		if err == nil && state.IMSReady && (!requireSMS || state.SMSReady) && (state.ICCID == "" || strings.EqualFold(state.ICCID, iccid)) {
			return nil
		}
		if err == nil && state.LastError != "" && !state.Active && !state.Enabled {
			return errors.New(state.LastError)
		}
		select {
		case <-ctx.Done():
			if err == nil && state.LastError != "" {
				return fmt.Errorf("wait for VoWiFi readiness: %s", state.LastError)
			}
			return fmt.Errorf("wait for VoWiFi readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) waitAutomaticVoWiFiStopped(ctx context.Context, deviceID string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, err := s.vowifi.State(deviceID)
		if err != nil || (!state.Active && !state.Enabled) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for VoWiFi shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) waitAutomaticCellular(ctx context.Context, physicalID string, requirePacketAttach bool) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	stableSamples := 0
	for {
		snapshot, err := s.devices.Refresh(ctx, physicalID)
		registered := err == nil && (snapshot.RegistrationStatus == 1 || snapshot.RegistrationStatus == 5)
		if registered && (!requirePacketAttach || snapshot.PSAttached) {
			stableSamples++
			if stableSamples >= 2 {
				return nil
			}
		} else {
			stableSamples = 0
		}
		if err == nil && snapshot.RegistrationStatus == 3 {
			return errors.New("cellular network registration was denied")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cellular registration: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) executeAutomaticSMS(ctx context.Context, task store.AutomaticTask, payload automaticTaskPayload) (string, error) {
	body, _ := json.Marshal(map[string]any{"device_id": task.DeviceID, "phone": payload.Phone, "message": payload.Message})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sms/send", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleSMSSend(recorder, request)
	if recorder.Code < 200 || recorder.Code >= 300 {
		failure := fmt.Errorf("send SMS failed (HTTP %d): %s", recorder.Code, compactAutomaticResponse(recorder.Body.Bytes()))
		// Once any part reached the modem/IMS transaction, retrying the whole
		// message could deliver a duplicate. Preparation failures remain safe to
		// retry according to the configured count.
		return "", automaticTaskExecutionError{err: failure, retryable: automaticSMSRetrySafe(recorder.Body.Bytes())}
	}
	return "短信已提交到 " + payload.Phone, nil
}

func automaticSMSRetrySafe(body []byte) bool {
	var payload struct {
		Data struct {
			PartsAttempted int   `json:"parts_attempted"`
			PartsAccepted  int   `json:"parts_accepted"`
			RetrySafe      *bool `json:"retry_safe"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if payload.Data.RetrySafe != nil {
		return *payload.Data.RetrySafe
	}
	return payload.Data.PartsAttempted == 0 && payload.Data.PartsAccepted == 0
}

func (s *Server) executeAutomaticCall(ctx context.Context, task store.AutomaticTask, payload automaticTaskPayload) (string, error) {
	config, err := s.store.Device(ctx, task.DeviceID)
	if err != nil {
		return "", err
	}
	_, physicalID, present := s.physicalForConfig(config)
	if !present {
		return "", errors.New("configured device is offline")
	}
	body, _ := json.Marshal(map[string]any{"number": payload.Phone, "duration_seconds": payload.DurationSeconds})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/devices/calls/dial", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.handleCallAction(recorder, request, config, physicalID, "dial")
	if recorder.Code < 200 || recorder.Code >= 300 {
		return "", fmt.Errorf("dial failed (HTTP %d): %s", recorder.Code, compactAutomaticResponse(recorder.Body.Bytes()))
	}
	return fmt.Sprintf("已拨打 %s，将在 %d 秒后自动挂断", payload.Phone, payload.DurationSeconds), nil
}

func (s *Server) executeAutomaticPublicIP(ctx context.Context, config store.Device, physicalID, iccid string, networkWasEnabled bool) (string, error) {
	if !networkWasEnabled {
		defer s.rollbackAutomaticNetwork(config.ID, physicalID, iccid, config)
	}
	if strings.TrimSpace(config.Interface) == "" {
		return "", errors.New("device has no cellular network interface")
	}
	info, err := exportproxy.LookupPublicIP(ctx, config.Interface)
	if err != nil {
		return "", fmt.Errorf("detect roaming public IP: %w", err)
	}
	s.savePublicIP(config.ID, iccid, info)
	return strings.TrimSpace(fmt.Sprintf("公网 IP %s · %s %s", info.IP, info.CountryCode, info.Region)), nil
}

func (s *Server) rollbackAutomaticNetwork(deviceID, physicalID, iccid string, config store.Device) {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.devices.SetNetwork(cleanupContext, physicalID, device.NetworkRequest{Enabled: false, APN: config.APN, IPVersion: "IPV4V6", Backend: config.DeviceBackend}); err != nil {
		s.logger.Warn("stop one-shot automatic roaming data", "device_id", deviceID, "error", err)
	}
	config.NetworkEnabled = false
	if err := s.store.UpsertDevice(cleanupContext, config); err != nil {
		s.logger.Warn("restore automatic roaming data setting", "device_id", deviceID, "error", err)
	}
	if err := s.store.UpsertCardPolicy(cleanupContext, store.CardPolicy{ICCID: iccid, NetworkEnabled: false, VoWiFiEnabled: false, AirplaneEnabled: false, APN: config.APN, IPVersion: "IPV4V6", Source: "automatic_task"}); err != nil {
		s.logger.Warn("restore automatic roaming card policy", "device_id", deviceID, "error", err)
	}
}

func compactAutomaticResponse(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if apiErr, ok := payload["error"].(map[string]any); ok {
			return firstNonEmpty(fmt.Sprint(apiErr["message"]), fmt.Sprint(apiErr["code"]), "request failed")
		}
	}
	return strings.TrimSpace(string(body))
}

func (s *Server) routeAutomaticTasksAPI(w http.ResponseWriter, r *http.Request, cleanPath string) bool {
	segments := splitAPIPath(cleanPath)
	if len(segments) == 0 || segments[0] != "automatic-tasks" {
		return false
	}
	if len(segments) == 1 {
		s.handleAutomaticTasks(w, r)
		return true
	}
	id, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_task_id", "automatic task ID is invalid")
		return true
	}
	if len(segments) == 2 {
		s.handleAutomaticTask(w, r, id)
		return true
	}
	if len(segments) == 3 && segments[2] == "run" {
		s.handleAutomaticTaskRunNow(w, r, id)
		return true
	}
	writeError(w, http.StatusNotFound, "not_found", "automatic task endpoint not found")
	return true
}

func (s *Server) handleAutomaticTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.store.ListAutomaticTasks(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		runs, err := s.store.ListAutomaticTaskRuns(r.Context(), 100)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"tasks": tasks, "runs": runs}})
	case http.MethodPost:
		task, err := s.decodeAutomaticTask(r, 0)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_automatic_task", err.Error())
			return
		}
		saved, err := s.store.SaveAutomaticTask(r.Context(), task)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": saved})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleAutomaticTask(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodPut:
		task, err := s.decodeAutomaticTask(r, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_automatic_task", err.Error())
			return
		}
		saved, err := s.store.SaveAutomaticTask(r.Context(), task)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": saved})
	case http.MethodDelete:
		if err := s.store.DeleteAutomaticTask(r.Context(), id); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleAutomaticTaskRunNow(w http.ResponseWriter, r *http.Request, id int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if s.automaticTasks == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "automatic task scheduler is unavailable")
		return
	}
	task, err := s.store.AutomaticTask(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	run, err := s.store.QueueAutomaticTaskNow(r.Context(), task)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.automaticTasks.enqueue(run)
	writeJSON(w, http.StatusAccepted, map[string]any{"data": run})
}

func (s *Server) decodeAutomaticTask(r *http.Request, id int64) (store.AutomaticTask, error) {
	var request struct {
		Name         string               `json:"name"`
		Enabled      bool                 `json:"enabled"`
		DeviceID     string               `json:"device_id"`
		ProfileICCID string               `json:"profile_iccid"`
		ProfileAID   string               `json:"profile_aid"`
		TaskType     string               `json:"task_type"`
		Environment  string               `json:"environment"`
		IntervalDays int                  `json:"interval_days"`
		StartDate    string               `json:"start_date"`
		RunTime      string               `json:"run_time"`
		Timezone     string               `json:"timezone"`
		RetryCount   int                  `json:"retry_count"`
		Notify       bool                 `json:"notify"`
		Payload      automaticTaskPayload `json:"payload"`
	}
	if err := s.decodeJSON(nilResponseWriter{}, r, &request); err != nil {
		return store.AutomaticTask{}, err
	}
	request.Name, request.DeviceID = strings.TrimSpace(request.Name), strings.TrimSpace(request.DeviceID)
	request.ProfileICCID, request.ProfileAID = strings.TrimSpace(request.ProfileICCID), strings.TrimSpace(request.ProfileAID)
	request.TaskType, request.Environment = strings.ToLower(strings.TrimSpace(request.TaskType)), strings.ToLower(strings.TrimSpace(request.Environment))
	if request.Name == "" || request.DeviceID == "" || request.ProfileICCID == "" {
		return store.AutomaticTask{}, errors.New("name, device, and eSIM profile are required")
	}
	if _, err := s.store.Device(r.Context(), request.DeviceID); err != nil {
		return store.AutomaticTask{}, errors.New("selected device does not exist")
	}
	if request.Environment != "vowifi" && request.Environment != "cellular" {
		return store.AutomaticTask{}, errors.New("environment must be vowifi or cellular")
	}
	if request.TaskType != "sms" && request.TaskType != "call" && request.TaskType != "public_ip" {
		return store.AutomaticTask{}, errors.New("unsupported task type")
	}
	if request.TaskType == "public_ip" && request.Environment != "cellular" {
		return store.AutomaticTask{}, errors.New("public IP tasks must use cellular direct mode")
	}
	if request.IntervalDays < 1 || request.IntervalDays > 365 || request.RetryCount < 0 || request.RetryCount > 10 {
		return store.AutomaticTask{}, errors.New("interval_days must be 1-365 and retry_count must be 0-10")
	}
	if request.TaskType == "sms" {
		if !validDialNumber(request.Payload.Phone) || strings.TrimSpace(request.Payload.Message) == "" {
			return store.AutomaticTask{}, errors.New("SMS phone and message are required")
		}
		if blocked, reason := blockedSMSDestination(request.Payload.Phone); blocked {
			return store.AutomaticTask{}, errors.New(reason)
		}
	}
	if request.TaskType == "call" && (!validDialNumber(request.Payload.Phone) || request.Payload.DurationSeconds < 1 || request.Payload.DurationSeconds > 600) {
		return store.AutomaticTask{}, errors.New("call phone is required and automatic hang-up must be 1-600 seconds")
	}
	request.Timezone = strings.TrimSpace(request.Timezone)
	if request.Timezone == "" {
		request.Timezone = time.Local.String()
	}
	location, err := time.LoadLocation(request.Timezone)
	if err != nil {
		return store.AutomaticTask{}, errors.New("timezone must be a valid IANA time zone")
	}
	nextRun, err := nextAutomaticRun(request.StartDate, request.RunTime, request.IntervalDays, time.Now().In(location))
	if err != nil {
		return store.AutomaticTask{}, err
	}
	payload, _ := json.Marshal(request.Payload)
	task := store.AutomaticTask{ID: id, Name: request.Name, Enabled: request.Enabled, DeviceID: request.DeviceID,
		ProfileICCID: request.ProfileICCID, ProfileAID: request.ProfileAID, TaskType: request.TaskType,
		Environment: request.Environment, IntervalDays: request.IntervalDays, StartDate: request.StartDate,
		RunTime: request.RunTime, Timezone: request.Timezone, Payload: payload, RetryCount: request.RetryCount, Notify: request.Notify, NextRunAt: nextRun.UTC()}
	if id != 0 {
		if previous, previousErr := s.store.AutomaticTask(r.Context(), id); previousErr == nil {
			task.CreatedAt, task.LastRunAt, task.LastStatus, task.LastError = previous.CreatedAt, previous.LastRunAt, previous.LastStatus, previous.LastError
		}
	}
	return task, nil
}

func nextAutomaticRun(date, clock string, intervalDays int, now time.Time) (time.Time, error) {
	location := now.Location()
	start, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(date)+" "+strings.TrimSpace(clock), location)
	if err != nil {
		return time.Time{}, errors.New("start_date and run_time must use YYYY-MM-DD and HH:MM")
	}
	for start.Before(now) {
		start = start.AddDate(0, 0, intervalDays)
	}
	return start, nil
}

// nilResponseWriter is used only because decodeJSON's size/error contract is
// shared with HTTP handlers; decode errors are returned to the real handler.
type nilResponseWriter struct{}

func (nilResponseWriter) Header() http.Header        { return make(http.Header) }
func (nilResponseWriter) Write([]byte) (int, error)  { return 0, nil }
func (nilResponseWriter) WriteHeader(statusCode int) {}
