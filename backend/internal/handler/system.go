package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/version"
)

// RegisterSystemRoutes registers /api/system endpoints
func RegisterSystemRoutes(r *gin.RouterGroup) {
	g := r.Group("/system")
	{
		g.GET("/scale", GetSystemScale)
		g.POST("/scale/refresh", RefreshSystemScale)
		g.GET("/warmup-status", GetWarmupStatus)
		g.GET("/indexes", GetIndexStatus)
		g.POST("/indexes/ensure", EnsureIndexes)
		g.GET("/update/status", GetUpdateStatus)
		g.POST("/update/check", CheckUpdate)
		g.POST("/update/run", RunUpdate)
	}
}

// GET /api/system/scale — placeholder until system_scale service is migrated
func GetSystemScale(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"scale": "medium",
			"metrics": gin.H{
				"total_users": 0,
				"total_logs":  0,
			},
			"settings": gin.H{
				"cache_ttl":                 300,
				"refresh_interval":          300,
				"frontend_refresh_interval": 60,
				"description":               "中型系统",
			},
		},
	})
}

// POST /api/system/scale/refresh
func RefreshSystemScale(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"scale":   "medium",
			"message": "Scale detection refreshed",
		},
	})
}

// GET /api/system/warmup-status
func GetWarmupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"status":   "ready",
			"progress": 100,
			"message":  "System is ready",
		},
	})
}

// GET /api/system/indexes
func GetIndexStatus(c *gin.Context) {
	db := database.Get()

	// Check existing indexes
	var indexes []struct {
		IndexName string `db:"indexname"`
	}

	var indexResults []gin.H
	total := 0
	existing := 0

	if db.IsPG {
		db.DB.Select(&indexes, "SELECT indexname FROM pg_indexes WHERE schemaname = 'public'")
	}

	// Build response matching Python format
	recommendedIndexes := []string{
		"idx_users_status",
		"idx_tokens_user_status",
		"idx_logs_created_type_user",
		"idx_logs_model_created",
		"idx_logs_token_created",
		"idx_logs_channel_created",
		"idx_redemptions_key",
		"idx_redemptions_status",
		"idx_top_ups_user",
		"idx_top_ups_status",
	}

	existingSet := make(map[string]bool)
	for _, idx := range indexes {
		existingSet[idx.IndexName] = true
	}

	for _, name := range recommendedIndexes {
		total++
		exists := existingSet[name]
		if exists {
			existing++
		}
		indexResults = append(indexResults, gin.H{
			"name":   name,
			"exists": exists,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"indexes":   indexResults,
			"total":     total,
			"existing":  existing,
			"missing":   total - existing,
			"all_ready": existing == total,
		},
	})
}

// POST /api/system/indexes/ensure
func EnsureIndexes(c *gin.Context) {
	db := database.Get()

	// Run index creation
	db.EnsureIndexes(true, 500*time.Millisecond)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"message": "Index creation completed",
		},
	})
}

// newUpdaterService lazily builds the updater service (nil-safe for tests).
var newUpdaterService = func() *service.UpdaterService {
	return service.NewUpdaterService()
}

// GetUpdateStatus handles GET /api/system/update/status.
// 返回一键更新的可用性与当前版本；未配置 watchtower 时 configured=false，
// 前端据此展示部署指引而不是更新按钮。
func GetUpdateStatus(c *gin.Context) {
	updater := newUpdaterService()
	status := updater.Status()

	remoteCommit := ""
	if !status.VersionIsDev {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		if sha, err := updater.LatestRemoteCommit(ctx); err == nil {
			remoteCommit = sha
		}
	}

	updateAvailable := false
	if remoteCommit != "" && version.GitCommit != "dev" && version.GitCommit != "" {
		// CI injects a full SHA. Keep prefix compatibility for older images that
		// may have been built with a short SHA, but compare in both directions so
		// malformed or unexpectedly short remote values cannot cause false updates.
		updateAvailable = !strings.HasPrefix(remoteCommit, version.GitCommit) &&
			!strings.HasPrefix(version.GitCommit, remoteCommit)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"configured":        status.Configured,
			"container_name":    status.ContainerName,
			"current_version":   status.CurrentVersion,
			"version_is_dev":    status.VersionIsDev,
			"current_image_ref": status.CurrentImageRef,
			"remote_commit":     remoteCommit,
			"update_available":  updateAvailable,
			"release_url":       version.ReleaseURL,
		},
	})
}

func updaterError(c *gin.Context, operation string, err error) {
	if errors.Is(err, service.ErrUpdaterNotConfigured) {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp(
			"UPDATER_NOT_CONFIGURED",
			"一键更新未配置：需在 .env 设置 WATCHTOWER_HTTP_API_TOKEN 并启用 watchtower sidecar",
			"参见 docker-compose.yml 中 watchtower 服务与 .env.example",
		))
		return
	}
	if errors.Is(err, service.ErrNoUpdateAvailable) {
		c.JSON(http.StatusConflict, models.ErrorResp(
			"NO_UPDATE_AVAILABLE",
			"当前镜像已是最新版本，无需更新",
			"",
		))
		return
	}

	// Keep sidecar URLs, registry responses and infrastructure details out of
	// the browser response. Full diagnostics remain in server logs.
	logger.L.Error("Updater "+operation+" failed: "+err.Error(), logger.CatSystem)
	code := "UPDATER_CHECK_FAILED"
	message := "检查更新失败，请查看服务端日志"
	if operation == "run" {
		code = "UPDATER_RUN_FAILED"
		message = "触发更新失败，请查看服务端日志"
	}
	c.JSON(http.StatusBadGateway, models.ErrorResp(code, message, ""))
}

// CheckUpdate handles POST /api/system/update/check.
// 经 Watchtower /v1/check 查询镜像 digest（不拉层），返回是否有更新。
func CheckUpdate(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	result, err := newUpdaterService().Check(ctx)
	if err != nil {
		updaterError(c, "check", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// RunUpdate handles POST /api/system/update/run.
// Self-update must be asynchronous: a successful recreate terminates this
// handler's container and therefore cannot preserve a synchronous response.
func RunUpdate(c *gin.Context) {
	const async = true

	updater := newUpdaterService()
	checkCtx, cancelCheck := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	checkResult, err := updater.Check(checkCtx)
	cancelCheck()
	if err != nil {
		updaterError(c, "run", err)
		return
	}
	updateAvailable, _ := checkResult["update_available"].(bool)
	if !updateAvailable {
		updaterError(c, "run", service.ErrNoUpdateAvailable)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	result, err := updater.Run(ctx, async)
	if err != nil {
		updaterError(c, "run", err)
		return
	}

	logger.L.Info("One-click update triggered asynchronously", logger.CatSystem)
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": result})
}
