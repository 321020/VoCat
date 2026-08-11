package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var wecomTemplateVariableNames = []string{
	"event",
	"title",
	"message",
	"timestamp",
	"content",
	"number",
	"device_id",
	"device_name",
	"device_label",
	"time",
}

type wecomTemplateValues map[string]string

func renderWecomPayload(template string, values wecomTemplateValues) ([]byte, error) {
	for _, name := range wecomTemplateVariableNames {
		encoded, err := json.Marshal(values[name])
		if err != nil {
			return nil, fmt.Errorf("encode WeCom template value %q: %w", name, err)
		}
		template = strings.ReplaceAll(template, "{{"+name+"}}", string(encoded))
	}
	if strings.Contains(template, "{{") {
		return nil, errors.New("wecom.payload_template contains an unsupported variable")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(template), &payload); err != nil || len(payload) == 0 {
		return nil, errors.New("wecom.payload_template must render to a non-empty JSON object")
	}
	return []byte(template), nil
}

func validateWecomResponse(status int, body []byte) error {
	var result struct {
		ErrCode *int `json:"errcode"`
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices ||
		json.Unmarshal(body, &result) != nil || result.ErrCode == nil || *result.ErrCode != 0 {
		return fmt.Errorf("%w: WeCom response was not successful", errProviderRejected)
	}
	return nil
}
