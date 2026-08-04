package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func parseFlowQuotaTimeRange(c *gin.Context) (int64, int64, bool) {
	startTimestamp, err := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	if err != nil || startTimestamp <= 0 {
		common.ApiErrorMsg(c, "invalid start_timestamp")
		return 0, 0, false
	}
	endTimestamp, err := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if err != nil || endTimestamp <= 0 {
		common.ApiErrorMsg(c, "invalid end_timestamp")
		return 0, 0, false
	}
	if endTimestamp < startTimestamp {
		common.ApiErrorMsg(c, "invalid time range")
		return 0, 0, false
	}
	return startTimestamp, endTimestamp, true
}

func GetAllQuotaDates(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	usernames := parseUsernameFilter(c.Query("username"))
	tokenIds := parseIntSliceFilter(c.Query("token_ids"))
	dates, err := model.GetAllQuotaDates(startTimestamp, endTimestamp, usernames, tokenIds)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
	return
}

func GetQuotaDatesByUser(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	dates, err := model.GetQuotaDataGroupByUser(startTimestamp, endTimestamp)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
}

func GetUserQuotaDates(c *gin.Context) {
	userId := c.GetInt("id")
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	// 判断时间跨度是否超过 1 个月
	if endTimestamp-startTimestamp > 2592000 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "时间跨度不能超过 1 个月",
		})
		return
	}
	tokenIds := parseIntSliceFilter(c.Query("token_ids"))
	dates, err := model.GetQuotaDataByUserId(userId, startTimestamp, endTimestamp, tokenIds)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
	return
}

func GetAllFlowQuotaDates(c *gin.Context) {
	startTimestamp, endTimestamp, ok := parseFlowQuotaTimeRange(c)
	if !ok {
		return
	}
	usernames := parseUsernameFilter(c.Query("username"))
	tokenIds := parseIntSliceFilter(c.Query("token_ids"))
	dates, err := model.GetFlowQuotaData(startTimestamp, endTimestamp, usernames, tokenIds, 0, c.GetInt("role"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
	return
}

func GetUserFlowQuotaDates(c *gin.Context) {
	userId := c.GetInt("id")
	startTimestamp, endTimestamp, ok := parseFlowQuotaTimeRange(c)
	if !ok {
		return
	}
	if endTimestamp-startTimestamp > 2592000 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "时间跨度不能超过 1 个月",
		})
		return
	}
	tokenIds := parseIntSliceFilter(c.Query("token_ids"))
	dates, err := model.GetFlowQuotaData(startTimestamp, endTimestamp, nil, tokenIds, userId, common.RoleCommonUser)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
	return
}

// parseUsernameFilter splits a comma-separated username query param into a
// trimmed, non-empty slice. Returns nil when no usernames are present.
func parseUsernameFilter(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	usernames := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name != "" {
			usernames = append(usernames, name)
		}
	}
	return usernames
}

// parseIntSliceFilter splits a comma-separated integer query param into a
// slice, ignoring invalid or non-positive values. The result is capped at
// 100 entries to keep IN queries bounded. Returns nil when empty.
func parseIntSliceFilter(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil || value <= 0 {
			continue
		}
		values = append(values, value)
		if len(values) >= 100 {
			break
		}
	}
	return values
}

// GetQuotaDatesByToken returns token-level quota data for admins.
// Accepts an optional comma-separated "username" query param to filter by users.
func GetQuotaDatesByToken(c *gin.Context) {
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	usernames := parseUsernameFilter(c.Query("username"))
	dates, err := model.GetQuotaDataGroupByToken(startTimestamp, endTimestamp, usernames)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
}

// GetQuotaTokenUsernames returns the distinct usernames that have token-level
// quota data. Admin only; used to populate the username filter options.
func GetQuotaTokenUsernames(c *gin.Context) {
	usernames, err := model.GetDistinctQuotaTokenUsernames()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    usernames,
	})
}

// GetQuotaDataUsernames returns the distinct usernames that have quota data.
// Admin only; used to populate the model-analytics username filter options.
func GetQuotaDataUsernames(c *gin.Context) {
	usernames, err := model.GetDistinctQuotaUsernames()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    usernames,
	})
}

// GetQuotaTokenOptions returns token options for the dashboard key filter.
// Normal users only see their own tokens; admins can search across all tokens
// by token name or the owner's username via the "keyword" query param.
func GetQuotaTokenOptions(c *gin.Context) {
	userId := c.GetInt("id")
	isAdmin := c.GetInt("role") >= common.RoleAdminUser
	keyword := c.Query("keyword")
	options, err := model.SearchTokenFilterOptions(userId, isAdmin, keyword, 50)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
}

// GetUserQuotaDatesByToken returns token-level quota data for the authenticated user only.
func GetUserQuotaDatesByToken(c *gin.Context) {
	userId := c.GetInt("id")
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if endTimestamp-startTimestamp > 2592000 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "时间跨度不能超过 1 个月",
		})
		return
	}
	dates, err := model.GetQuotaDataByUserToken(userId, startTimestamp, endTimestamp)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dates,
	})
}
