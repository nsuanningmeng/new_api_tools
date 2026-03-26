package service

import (
	"fmt"
	"math"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

// Constants for model status
var (
	AvailableTimeWindows = []string{"1h", "6h", "12h", "24h"}
	DefaultTimeWindow    = "24h"
	AvailableThemes = []string{
		"daylight", "obsidian", "minimal", "neon", "forest", "ocean", "terminal",
		"cupertino", "material", "openai", "anthropic", "vercel", "linear",
		"stripe", "github", "discord", "tesla",
	}
	DefaultTheme = "daylight"
	// LegacyThemeMap maps old theme names to valid ones
	LegacyThemeMap = map[string]string{
		"light":  "daylight",
		"dark":   "obsidian",
		"system": "daylight",
	}
	AvailableRefreshIntervals = []int{0, 30, 60, 120, 300}
	AvailableSortModes        = []string{"default", "availability", "custom"}
)

// Time window slot configurations: {totalSeconds, numSlots, slotSeconds}
// Must match Python backend and frontend TIME_WINDOWS exactly
type timeWindowConfig struct {
	totalSeconds int64
	numSlots     int
	slotSeconds  int64
}

var timeWindowConfigs = map[string]timeWindowConfig{
	"1h":  {3600, 60, 60},    // 1 hour, 60 slots, 1 minute each
	"6h":  {21600, 24, 900},  // 6 hours, 24 slots, 15 minutes each
	"12h": {43200, 24, 1800}, // 12 hours, 24 slots, 30 minutes each
	"24h": {86400, 24, 3600}, // 24 hours, 24 slots, 1 hour each
}

// getStatusColor determines status color based on success rate (matches Python backend)
func getStatusColor(successRate float64, totalRequests int64) string {
	if totalRequests == 0 {
		return "green" // No requests = no issues
	}
	if successRate >= 95 {
		return "green"
	} else if successRate >= 80 {
		return "yellow"
	}
	return "red"
}

// roundRate rounds a float to 2 decimal places
func roundRate(rate float64) float64 {
	return math.Round(rate*100) / 100
}

// isFailureByStatusCode checks if a request should be counted as failure
// Only 500, 429, 503 are considered failures; all others are success
func isFailureByStatusCode(statusCode int) bool {
	return statusCode == 500 || statusCode == 429 || statusCode == 503
}

// buildStatusCodeExpr builds SQL expression to extract status_code from other JSON field
func buildStatusCodeExpr(isPG bool) string {
	if isPG {
		return "CAST(other::jsonb->>'status_code' AS INTEGER)"
	}
	return "CAST(JSON_UNQUOTE(JSON_EXTRACT(other, '$.status_code')) AS UNSIGNED)"
}

// buildSuccessCondition builds SQL CASE expression for success counting with status code fallback
func buildSuccessCondition(isPG bool) string {
	statusCodeExpr := buildStatusCodeExpr(isPG)
	return fmt.Sprintf(`CASE
		WHEN other IS NOT NULL AND other != '' AND %s IS NOT NULL
		THEN CASE WHEN %s NOT IN (429, 500, 503) THEN 1 ELSE 0 END
		ELSE CASE WHEN type = 2 THEN 1 ELSE 0 END
	END`, statusCodeExpr, statusCodeExpr)
}

// ModelStatusService handles model availability monitoring
type ModelStatusService struct {
	db *database.Manager
}

// NewModelStatusService creates a new ModelStatusService
func NewModelStatusService() *ModelStatusService {
	return &ModelStatusService{db: database.Get()}
}

// GetAvailableModels returns all models with 24h request counts
func (s *ModelStatusService) GetAvailableModels() ([]map[string]interface{}, error) {
	cm := cache.Get()
	var cached []map[string]interface{}
	found, _ := cm.GetJSON("model_status:available_models", &cached)
	if found {
		return cached, nil
	}

	startTime := time.Now().Unix() - 86400

	query := s.db.RebindQuery(`
		SELECT model_name, COUNT(*) as request_count_24h
		FROM (
			SELECT model_name,
				ROW_NUMBER() OVER (PARTITION BY request_id ORDER BY id DESC) as rn
			FROM logs
			WHERE type IN (2, 5) AND model_name != '' AND created_at >= ? AND request_id != ''
		) latest
		WHERE rn = 1
		GROUP BY model_name
		ORDER BY request_count_24h DESC`)

	rows, err := s.db.Query(query, startTime)
	if err != nil {
		return nil, err
	}

	cm.Set("model_status:available_models", rows, 5*time.Minute)
	return rows, nil
}

// GetModelStatus returns status for a specific model
// Uses a single GROUP BY FLOOR query (matches Python backend optimization)
func (s *ModelStatusService) GetModelStatus(modelName, window string) (map[string]interface{}, error) {
	cacheKey := fmt.Sprintf("model_status:%s:%s", modelName, window)
	cm := cache.Get()
	var cached map[string]interface{}
	found, _ := cm.GetJSON(cacheKey, &cached)
	if found {
		return cached, nil
	}

	// Get window configuration (dynamic slot count per window)
	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs["24h"]
	}

	now := time.Now().Unix()
	startTime := now - twConfig.totalSeconds
	numSlots := twConfig.numSlots
	slotSeconds := twConfig.slotSeconds

	// Single optimized query — aggregate by time slot using FLOOR division
	// Only count the last record per request_id (final result after retries)
	slotQuery := s.db.RebindQuery(fmt.Sprintf(`
		SELECT FLOOR((created_at - %d) / %d) as slot_idx,
			COUNT(*) as total,
			SUM(CASE WHEN type = 2 THEN 1 ELSE 0 END) as success
		FROM (
			SELECT created_at, type,
				ROW_NUMBER() OVER (PARTITION BY request_id ORDER BY id DESC) as rn
			FROM logs
			WHERE model_name = ?
				AND created_at >= ? AND created_at < ?
				AND type IN (2, 5)
				AND request_id != ''
		) latest
		WHERE rn = 1
		GROUP BY FLOOR((created_at - %d) / %d)`,
		startTime, slotSeconds,
		startTime, slotSeconds))

	rows, _ := s.db.Query(slotQuery, modelName, startTime, now)

	// Initialize all slots with zeros
	type slotInfo struct {
		total   int64
		success int64
	}
	slotMap := make(map[int64]*slotInfo, numSlots)

	// Fill in actual data from query results
	if rows != nil {
		for _, row := range rows {
			idx := toInt64(row["slot_idx"])
			if idx >= 0 && idx < int64(numSlots) {
				slotMap[idx] = &slotInfo{
					total:   toInt64(row["total"]),
					success: toInt64(row["success"]),
				}
			}
		}
	}

	// Build slot_data list with status colors
	slotData := make([]map[string]interface{}, 0, numSlots)
	totalReqs := int64(0)
	totalSuccess := int64(0)

	for i := 0; i < numSlots; i++ {
		slotStart := startTime + int64(i)*slotSeconds
		slotEnd := slotStart + slotSeconds

		si := slotMap[int64(i)]
		slotTotal := int64(0)
		slotSuccess := int64(0)
		if si != nil {
			slotTotal = si.total
			slotSuccess = si.success
		}

		slotRate := float64(100)
		if slotTotal > 0 {
			slotRate = float64(slotSuccess) / float64(slotTotal) * 100
		}

		slotData = append(slotData, map[string]interface{}{
			"slot":           i,
			"start_time":     slotStart,
			"end_time":       slotEnd,
			"total_requests": slotTotal,
			"success_count":  slotSuccess,
			"success_rate":   roundRate(slotRate),
			"status":         getStatusColor(slotRate, slotTotal),
		})

		totalReqs += slotTotal
		totalSuccess += slotSuccess
	}

	overallRate := float64(100)
	if totalReqs > 0 {
		overallRate = float64(totalSuccess) / float64(totalReqs) * 100
	}

	result := map[string]interface{}{
		"model_name":     modelName,
		"display_name":   modelName,
		"time_window":    window,
		"total_requests": totalReqs,
		"success_count":  totalSuccess,
		"success_rate":   roundRate(overallRate),
		"current_status": getStatusColor(overallRate, totalReqs),
		"slot_data":      slotData,
	}

	cm.Set(cacheKey, result, 30*time.Second)
	return result, nil
}

// GetMultipleModelsStatus returns status for multiple models
func (s *ModelStatusService) GetMultipleModelsStatus(modelNames []string, window string) ([]map[string]interface{}, error) {
	results := make([]map[string]interface{}, 0, len(modelNames))
	for _, name := range modelNames {
		status, err := s.GetModelStatus(name, window)
		if err != nil {
			continue
		}
		results = append(results, status)
	}
	return results, nil
}

// GetAllModelsStatus returns status for all models that have requests
func (s *ModelStatusService) GetAllModelsStatus(window string) ([]map[string]interface{}, error) {
	models, err := s.GetAvailableModels()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(models))
	for _, m := range models {
		if name, ok := m["model_name"].(string); ok {
			names = append(names, name)
		}
	}

	return s.GetMultipleModelsStatus(names, window)
}

// Config management via cache

// GetSelectedModels returns selected model names from cache
func (s *ModelStatusService) GetSelectedModels() []string {
	cm := cache.Get()
	var models []string
	found, _ := cm.GetJSON("model_status:selected_models", &models)
	if found {
		return models
	}
	return []string{}
}

// SetSelectedModels saves selected models to cache
func (s *ModelStatusService) SetSelectedModels(models []string) {
	cm := cache.Get()
	cm.Set("model_status:selected_models", models, 0) // no expiry
}

// GetConfig returns all model status config
func (s *ModelStatusService) GetConfig() map[string]interface{} {
	cm := cache.Get()

	var timeWindow string
	found, _ := cm.GetJSON("model_status:time_window", &timeWindow)
	if !found {
		timeWindow = DefaultTimeWindow
	}

	var theme string
	found, _ = cm.GetJSON("model_status:theme", &theme)
	if !found {
		theme = DefaultTheme
	}
	// Map legacy theme names to valid ones
	if mapped, ok := LegacyThemeMap[theme]; ok {
		theme = mapped
	}

	var refreshInterval int
	found, _ = cm.GetJSON("model_status:refresh_interval", &refreshInterval)
	if !found {
		refreshInterval = 60
	}

	var sortMode string
	found, _ = cm.GetJSON("model_status:sort_mode", &sortMode)
	if !found {
		sortMode = "default"
	}

	var customOrder []string
	cm.GetJSON("model_status:custom_order", &customOrder)

	var customGroups []map[string]interface{}
	found, _ = cm.GetJSON("model_status:custom_groups", &customGroups)
	if !found {
		customGroups = []map[string]interface{}{}
	}

	return map[string]interface{}{
		"time_window":      timeWindow,
		"theme":            theme,
		"refresh_interval": refreshInterval,
		"sort_mode":        sortMode,
		"custom_order":     customOrder,
		"selected_models":  s.GetSelectedModels(),
		"custom_groups":    customGroups,
		"site_title":       s.GetSiteTitle(),
	}
}

// SetTimeWindow saves time window to cache
func (s *ModelStatusService) SetTimeWindow(window string) {
	cm := cache.Get()
	cm.Set("model_status:time_window", window, 0)
}

// SetTheme saves theme to cache
func (s *ModelStatusService) SetTheme(theme string) {
	cm := cache.Get()
	cm.Set("model_status:theme", theme, 0)
}

// SetRefreshInterval saves refresh interval to cache
func (s *ModelStatusService) SetRefreshInterval(interval int) {
	cm := cache.Get()
	cm.Set("model_status:refresh_interval", interval, 0)
}

// SetSortMode saves sort mode to cache
func (s *ModelStatusService) SetSortMode(mode string) {
	cm := cache.Get()
	cm.Set("model_status:sort_mode", mode, 0)
}

// SetCustomOrder saves custom order to cache
func (s *ModelStatusService) SetCustomOrder(order []string) {
	cm := cache.Get()
	cm.Set("model_status:custom_order", order, 0)
}

// GetCustomGroups returns custom model groups from cache
func (s *ModelStatusService) GetCustomGroups() []map[string]interface{} {
	cm := cache.Get()
	var groups []map[string]interface{}
	found, _ := cm.GetJSON("model_status:custom_groups", &groups)
	if found {
		return groups
	}
	return []map[string]interface{}{}
}

// SetCustomGroups saves custom model groups to cache
func (s *ModelStatusService) SetCustomGroups(groups []map[string]interface{}) {
	cm := cache.Get()
	cm.Set("model_status:custom_groups", groups, 0) // no expiry
}

// GetSiteTitle returns the custom site title
func (s *ModelStatusService) GetSiteTitle() string {
	cm := cache.Get()
	var title string
	found, _ := cm.GetJSON("model_status:site_title", &title)
	if found {
		return title
	}
	return ""
}

// SetSiteTitle saves the custom site title
func (s *ModelStatusService) SetSiteTitle(title string) {
	cm := cache.Get()
	cm.Set("model_status:site_title", title, 0)
}

// GetEmbedConfig returns embed page configuration
func (s *ModelStatusService) GetEmbedConfig() map[string]interface{} {
	config := s.GetConfig()
	config["available_time_windows"] = AvailableTimeWindows
	config["available_themes"] = AvailableThemes
	config["available_refresh_intervals"] = AvailableRefreshIntervals
	config["available_sort_modes"] = AvailableSortModes
	return config
}

// GetModelGroupStatus returns status for a specific model grouped by group field
func (s *ModelStatusService) GetModelGroupStatus(modelName, window string) ([]map[string]interface{}, error) {
	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs["24h"]
	}

	now := time.Now().Unix()
	startTime := now - twConfig.totalSeconds

	// Query to get all groups for this model (using 'group' field, not 'group_name')
	groupQuery := s.db.RebindQuery(`
		SELECT DISTINCT group_col
		FROM (
			SELECT ` + "`group`" + ` as group_col,
				ROW_NUMBER() OVER (PARTITION BY request_id ORDER BY id DESC) as rn
			FROM logs
			WHERE model_name = ?
				AND created_at >= ? AND created_at < ?
				AND type IN (2, 5)
				AND request_id != ''
				AND ` + "`group`" + ` != ''
		) latest
		WHERE rn = 1`)

	groupRows, err := s.db.Query(groupQuery, modelName, startTime, now)
	if err != nil {
		return nil, err
	}

	results := make([]map[string]interface{}, 0)
	for _, groupRow := range groupRows {
		groupName := toString(groupRow["group_col"])
		if groupName == "" {
			continue
		}

		status, err := s.getModelGroupStatusDetail(modelName, groupName, window)
		if err != nil {
			continue
		}
		results = append(results, status)
	}

	return results, nil
}

// getModelGroupStatusDetail returns detailed status for a specific model+group
func (s *ModelStatusService) getModelGroupStatusDetail(modelName, groupName, window string) (map[string]interface{}, error) {
	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs["24h"]
	}

	now := time.Now().Unix()
	startTime := now - twConfig.totalSeconds
	numSlots := twConfig.numSlots
	slotSeconds := twConfig.slotSeconds

	slotQuery := s.db.RebindQuery(fmt.Sprintf(`
		SELECT FLOOR((created_at - %d) / %d) as slot_idx,
			COUNT(*) as total,
			SUM(CASE WHEN type = 2 THEN 1 ELSE 0 END) as success
		FROM (
			SELECT created_at, type,
				ROW_NUMBER() OVER (PARTITION BY request_id ORDER BY id DESC) as rn
			FROM logs
			WHERE model_name = ? AND `+"`group`"+` = ?
				AND created_at >= ? AND created_at < ?
				AND type IN (2, 5)
				AND request_id != ''
		) latest
		WHERE rn = 1
		GROUP BY FLOOR((created_at - %d) / %d)`,
		startTime, slotSeconds,
		startTime, slotSeconds))

	rows, _ := s.db.Query(slotQuery, modelName, groupName, startTime, now)

	type slotInfo struct {
		total   int64
		success int64
	}
	slotMap := make(map[int64]*slotInfo, numSlots)

	if rows != nil {
		for _, row := range rows {
			idx := toInt64(row["slot_idx"])
			if idx >= 0 && idx < int64(numSlots) {
				slotMap[idx] = &slotInfo{
					total:   toInt64(row["total"]),
					success: toInt64(row["success"]),
				}
			}
		}
	}

	slotData := make([]map[string]interface{}, 0, numSlots)
	totalReqs := int64(0)
	totalSuccess := int64(0)

	for i := 0; i < numSlots; i++ {
		slotStart := startTime + int64(i)*slotSeconds
		slotEnd := slotStart + slotSeconds

		si := slotMap[int64(i)]
		slotTotal := int64(0)
		slotSuccess := int64(0)
		if si != nil {
			slotTotal = si.total
			slotSuccess = si.success
		}

		slotRate := float64(100)
		if slotTotal > 0 {
			slotRate = float64(slotSuccess) / float64(slotTotal) * 100
		}

		slotData = append(slotData, map[string]interface{}{
			"slot":           i,
			"start_time":     slotStart,
			"end_time":       slotEnd,
			"total_requests": slotTotal,
			"success_count":  slotSuccess,
			"success_rate":   roundRate(slotRate),
			"status":         getStatusColor(slotRate, slotTotal),
		})

		totalReqs += slotTotal
		totalSuccess += slotSuccess
	}

	overallRate := float64(100)
	if totalReqs > 0 {
		overallRate = float64(totalSuccess) / float64(totalReqs) * 100
	}

	return map[string]interface{}{
		"group_name":     groupName,
		"model_name":     modelName,
		"time_window":    window,
		"total_requests": totalReqs,
		"success_count":  totalSuccess,
		"success_rate":   roundRate(overallRate),
		"current_status": getStatusColor(overallRate, totalReqs),
		"slot_data":      slotData,
	}, nil
}

// GetSelectedGroups returns selected group names from cache
func (s *ModelStatusService) GetSelectedGroups() []string {
	cm := cache.Get()
	var groups []string
	found, _ := cm.GetJSON("model_status:selected_groups", &groups)
	if found {
		return groups
	}
	return []string{}
}

// SetSelectedGroups saves selected groups to cache
func (s *ModelStatusService) SetSelectedGroups(groups []string) {
	cm := cache.Get()
	cm.Set("model_status:selected_groups", groups, 0)
}

// GetBatchSummary calculates average success rate excluding groups with keyword
func (s *ModelStatusService) GetBatchSummary(modelNames []string, window string, excludeKeyword string) (map[string]interface{}, error) {
	if len(modelNames) == 0 {
		return map[string]interface{}{
			"average_success_rate": 0,
			"excluded_keyword":     excludeKeyword,
			"models_count":         0,
		}, nil
	}

	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs["24h"]
	}

	now := time.Now().Unix()
	startTime := now - twConfig.totalSeconds

	// Build placeholders for IN clause
	placeholders := ""
	args := []interface{}{}
	for i, name := range modelNames {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, name)
	}
	args = append(args, startTime, now, "%"+excludeKeyword+"%")

	successCondition := buildSuccessCondition(s.db.IsPG)

	query := s.db.RebindQuery(fmt.Sprintf(`
		SELECT model_name,
			COUNT(*) as total,
			SUM(%s) as success
		FROM (
			SELECT model_name, type, other, ` + "`group`" + `,
				ROW_NUMBER() OVER (PARTITION BY request_id ORDER BY id DESC) as rn
			FROM logs
			WHERE model_name IN (%s)
				AND created_at >= ? AND created_at < ?
				AND type IN (2, 5)
				AND request_id != ''
				AND (` + "`group`" + ` NOT LIKE ? OR ` + "`group`" + ` IS NULL OR ` + "`group`" + ` = '')
		) latest
		WHERE rn = 1
		GROUP BY model_name`, successCondition, placeholders))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}

	totalSuccessRate := 0.0
	modelsWithRequests := 0

	for _, row := range rows {
		total := toInt64(row["total"])
		success := toInt64(row["success"])
		if total > 0 {
			rate := float64(success) / float64(total) * 100
			totalSuccessRate += rate
			modelsWithRequests++
		}
	}

	avgRate := 0.0
	if modelsWithRequests > 0 {
		avgRate = totalSuccessRate / float64(modelsWithRequests)
	}

	return map[string]interface{}{
		"average_success_rate": roundRate(avgRate),
		"excluded_keyword":     excludeKeyword,
		"models_count":         modelsWithRequests,
		"failure_status_codes": []int{429, 500, 503},
	}, nil
}
