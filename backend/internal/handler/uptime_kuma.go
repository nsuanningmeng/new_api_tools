package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterUptimeKumaRoutes registers public Uptime Kuma compatible endpoints
func RegisterUptimeKumaRoutes(r *gin.Engine) {
	g := r.Group("/api/uptime-kuma")
	{
		g.GET("/monitor/:model_name", GetUptimeKumaMonitor)
		g.GET("/monitors", GetUptimeKumaMonitors)
		g.POST("/monitors/batch", GetUptimeKumaMonitorsBatch)
		g.GET("/status-page", GetUptimeKumaStatusPage)
		g.GET("/status-page/:slug", GetUptimeKumaStatusPageWithSlug)
	}
}

// GET /api/uptime-kuma/monitor/:model_name
func GetUptimeKumaMonitor(c *gin.Context) {
	modelName := c.Param("model_name")
	window := c.DefaultQuery("window", service.DefaultTimeWindow)

	svc := service.NewUptimeKumaService()
	data, err := svc.GetMonitor(modelName, window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, data)
}

// GET /api/uptime-kuma/monitors
func GetUptimeKumaMonitors(c *gin.Context) {
	window := c.DefaultQuery("window", service.DefaultTimeWindow)

	svc := service.NewUptimeKumaService()
	data, err := svc.GetMonitors(window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, gin.H{"monitors": data})
}

// POST /api/uptime-kuma/monitors/batch
func GetUptimeKumaMonitorsBatch(c *gin.Context) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}

	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	svc := service.NewUptimeKumaService()

	monitors := make([]map[string]interface{}, 0)
	for _, modelName := range req.Models {
		monitor, err := svc.GetMonitor(modelName, window)
		if err != nil {
			continue
		}
		monitors = append(monitors, monitor)
	}

	c.JSON(http.StatusOK, gin.H{"monitors": monitors})
}

// GET /api/uptime-kuma/status-page
func GetUptimeKumaStatusPage(c *gin.Context) {
	window := c.DefaultQuery("window", service.DefaultTimeWindow)

	svc := service.NewUptimeKumaService()
	data, err := svc.GetStatusPage(window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, data)
}

// GET /api/uptime-kuma/status-page/:slug
func GetUptimeKumaStatusPageWithSlug(c *gin.Context) {
	// Ignore slug, always return the same status page
	window := c.DefaultQuery("window", service.DefaultTimeWindow)

	svc := service.NewUptimeKumaService()
	data, err := svc.GetStatusPage(window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, data)
}
