package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxInspectableMCPBodyBytes = 1 << 20

type mcpRequestObservation struct {
	Method    string
	ToolName  string
	Prompt    string
	Resource  string
	Batch     bool
	BatchSize int
	Methods   []string
	ToolNames []string
	Prompts   []string
	Resources []string
}

func observeMCPRequest(r *http.Request) (mcpRequestObservation, *http.Request, error) {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return mcpRequestObservation{}, r, nil
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "json") {
		return mcpRequestObservation{}, r, nil
	}

	if r.ContentLength < 0 || r.ContentLength > maxInspectableMCPBodyBytes {
		return mcpRequestObservation{}, r, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return mcpRequestObservation{}, restoreRequestBody(r, body), fmt.Errorf("read mcp request body: %w", err)
	}
	if int64(len(body)) > maxInspectableMCPBodyBytes {
		return mcpRequestObservation{}, restoreRequestBody(r, body), fmt.Errorf("mcp request body exceeds inspection limit")
	}
	r = restoreRequestBody(r, body)
	if len(bytes.TrimSpace(body)) == 0 {
		return mcpRequestObservation{}, r, nil
	}

	observation, err := decodeMCPObservation(body)
	if err != nil {
		return mcpRequestObservation{}, r, err
	}
	return observation, r, nil
}

func restoreRequestBody(r *http.Request, body []byte) *http.Request {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return r
}

func decodeMCPObservation(body []byte) (mcpRequestObservation, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return mcpRequestObservation{}, nil
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return mcpRequestObservation{}, fmt.Errorf("decode mcp batch request: %w", err)
		}
		observation := mcpRequestObservation{Batch: true, BatchSize: len(batch)}
		for _, item := range batch {
			itemObservation, err := decodeSingleMCPObservation(item)
			if err != nil {
				return mcpRequestObservation{}, err
			}
			observation.merge(itemObservation)
		}
		return observation, nil
	}
	return decodeSingleMCPObservation(trimmed)
}

func (o *mcpRequestObservation) merge(item mcpRequestObservation) {
	if item.Method != "" {
		if o.Method == "" {
			o.Method = item.Method
		}
		o.Methods = append(o.Methods, item.Method)
	}
	if item.ToolName != "" {
		if o.ToolName == "" {
			o.ToolName = item.ToolName
		}
		o.ToolNames = append(o.ToolNames, item.ToolName)
	}
	if item.Prompt != "" {
		if o.Prompt == "" {
			o.Prompt = item.Prompt
		}
		o.Prompts = append(o.Prompts, item.Prompt)
	}
	if item.Resource != "" {
		if o.Resource == "" {
			o.Resource = item.Resource
		}
		o.Resources = append(o.Resources, item.Resource)
	}
}

func decodeSingleMCPObservation(body []byte) (mcpRequestObservation, error) {
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return mcpRequestObservation{}, fmt.Errorf("decode mcp request: %w", err)
	}
	observation := mcpRequestObservation{Method: strings.TrimSpace(envelope.Method)}
	if len(envelope.Params) == 0 {
		return observation, nil
	}
	var params map[string]any
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		return observation, nil
	}
	switch observation.Method {
	case "tools/call":
		observation.ToolName = stringParam(params, "name")
	case "prompts/get":
		observation.Prompt = stringParam(params, "name")
	case "resources/read":
		observation.Resource = stringParam(params, "uri")
	}
	observation.Methods = append(observation.Methods, observation.Method)
	if observation.ToolName != "" {
		observation.ToolNames = append(observation.ToolNames, observation.ToolName)
	}
	if observation.Prompt != "" {
		observation.Prompts = append(observation.Prompts, observation.Prompt)
	}
	if observation.Resource != "" {
		observation.Resources = append(observation.Resources, observation.Resource)
	}
	return observation, nil
}

func stringParam(params map[string]any, key string) string {
	value, ok := params[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
