package service

import (
	"fmt"
	"time"
)

// UptimeKumaService converts model status to Uptime Kuma format
type UptimeKumaService struct {
	modelStatus *ModelStatusService
}

// NewUptimeKumaService creates a new UptimeKumaService
func NewUptimeKumaService() *UptimeKumaService {
	return &UptimeKumaService{
		modelStatus: NewModelStatusService(),
	}
}

// statusToUptimeKuma converts internal status to Uptime Kuma status
// green -> 1 (UP), yellow -> 2 (PENDING), red -> 0 (DOWN)
func statusToUptimeKuma(status string) int {
	switch status {
	case "green":
		return 1
	case "yellow":
		return 2
	case "red":
		return 0
	default:
		return 2
	}
}

// GetMonitor returns a single monitor in Uptime Kuma format
func (s *UptimeKumaService) GetMonitor(modelName, window string) (map[string]interface{}, error) {
	status, err := s.modelStatus.GetModelStatus(modelName, window)
	if err != nil {
		return nil, err
	}

	// Build heartbeat array
	heartbeats := make([]map[string]interface{}, 0)
	if slotData, ok := status["slot_data"].([]map[string]interface{}); ok {
		for _, slot := range slotData {
			startTime := toInt64(slot["start_time"])
			slotStatus := ""
			if s, ok := slot["status"].(string); ok {
				slotStatus = s
			}

			heartbeats = append(heartbeats, map[string]interface{}{
				"status": statusToUptimeKuma(slotStatus),
				"time":   time.Unix(startTime, 0).Format(time.RFC3339),
				"ping":   0,
				"msg":    "",
			})
		}
	}

	successRate := 0.0
	if sr, ok := status["success_rate"].(float64); ok {
		successRate = sr
	}

	return map[string]interface{}{
		"id":     fmt.Sprintf("model-%s", modelName),
		"name":   modelName,
		"type":   "http",
		"status": statusToUptimeKuma(toString(status["current_status"])),
		"uptime": map[string]interface{}{
			"24h": successRate,
			"30d": successRate,
			"1y":  successRate,
		},
		"heartbeat": heartbeats,
	}, nil
}

// GetMonitors returns all monitors in Uptime Kuma format
func (s *UptimeKumaService) GetMonitors(window string) ([]map[string]interface{}, error) {
	models, err := s.modelStatus.GetAvailableModels()
	if err != nil {
		return nil, err
	}

	monitors := make([]map[string]interface{}, 0)
	for _, m := range models {
		modelName := toString(m["model_name"])
		if modelName == "" {
			continue
		}

		monitor, err := s.GetMonitor(modelName, window)
		if err != nil {
			continue
		}
		monitors = append(monitors, monitor)
	}

	return monitors, nil
}

// GetStatusPage returns status page data in Uptime Kuma format
func (s *UptimeKumaService) GetStatusPage(window string) (map[string]interface{}, error) {
	monitors, err := s.GetMonitors(window)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"title":       "Model Status",
		"description": "AI Model Availability Monitor",
		"monitors":    monitors,
		"incident":    []interface{}{},
	}, nil
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
