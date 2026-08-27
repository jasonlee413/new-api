package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenModelQuotaResponse struct {
	Success bool                  `json:"success"`
	Message string                `json:"message"`
	Data    []model.ModelQuotaData `json:"data"`
}

func setupTokenModelControllerTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.QuotaData{}))
	seed := []model.QuotaData{
		{UserID: 1, Username: "alice", TokenID: 11, ModelName: "gpt-a", CreatedAt: 1100, Count: 2, Quota: 100, TokenUsed: 40},
		{UserID: 1, Username: "alice", TokenID: 11, ModelName: "gpt-a", CreatedAt: 1200, Count: 1, Quota: 30, TokenUsed: 10},
		{UserID: 1, Username: "alice", TokenID: 11, ModelName: "gpt-b", CreatedAt: 1100, Count: 1, Quota: 50, TokenUsed: 20},
		{UserID: 2, Username: "bob", TokenID: 22, ModelName: "gpt-a", CreatedAt: 1100, Count: 1, Quota: 70, TokenUsed: 30},
	}
	for _, row := range seed {
		row := row
		require.NoError(t, model.DB.Create(&row).Error)
	}
}

func decodeTokenModelQuotaResponse(t *testing.T, recorder *httptest.ResponseRecorder) tokenModelQuotaResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload tokenModelQuotaResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload
}

func TestGetTokenModelQuotaDataAggregatesByModel(t *testing.T) {
	setupTokenModelControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleAdminUser)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/tokens/models?token_id=11&start_timestamp=1000&end_timestamp=2000", nil)

	GetTokenModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.True(t, payload.Success, payload.Message)
	require.Len(t, payload.Data, 2)
	// ordered by quota DESC: gpt-a (100+30) first, then gpt-b
	assert.Equal(t, "gpt-a", payload.Data[0].ModelName)
	assert.Equal(t, 3, payload.Data[0].Count)
	assert.Equal(t, 130, payload.Data[0].Quota)
	assert.Equal(t, 50, payload.Data[0].TokenUsed)
	assert.Equal(t, "gpt-b", payload.Data[1].ModelName)
	assert.Equal(t, 1, payload.Data[1].Count)
	assert.Equal(t, 50, payload.Data[1].Quota)
	assert.Equal(t, 20, payload.Data[1].TokenUsed)
}

func TestGetTokenModelQuotaDataRejectsInvalidTokenId(t *testing.T) {
	setupTokenModelControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleAdminUser)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/tokens/models?token_id=0&start_timestamp=1000&end_timestamp=2000", nil)

	GetTokenModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.False(t, payload.Success)
	assert.Equal(t, "invalid token_id", payload.Message)
}

func TestGetUserTokenModelQuotaDataRestrictsToAuthenticatedUser(t *testing.T) {
	setupTokenModelControllerTestDB(t)

	// user 1 queries token 22 which belongs to user 2: must get an empty result
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 1)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/tokens/self/models?token_id=22&start_timestamp=1000&end_timestamp=2000", nil)

	GetUserTokenModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.True(t, payload.Success, payload.Message)
	assert.Empty(t, payload.Data)
}

func TestGetUserTokenModelQuotaDataReturnsOwnTokenModels(t *testing.T) {
	setupTokenModelControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 1)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/tokens/self/models?token_id=11&start_timestamp=1000&end_timestamp=2000", nil)

	GetUserTokenModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.True(t, payload.Success, payload.Message)
	require.Len(t, payload.Data, 2)
	assert.Equal(t, "gpt-a", payload.Data[0].ModelName)
	assert.Equal(t, 130, payload.Data[0].Quota)
}

func TestGetUserModelQuotaDataAggregatesAcrossTokens(t *testing.T) {
	setupTokenModelControllerTestDB(t)
	// alice's second key also consumed gpt-b; the user drill-down must merge
	// consumption across all of her keys
	require.NoError(t, model.DB.Create(&model.QuotaData{
		UserID: 1, Username: "alice", TokenID: 33, ModelName: "gpt-b",
		CreatedAt: 1300, Count: 2, Quota: 10, TokenUsed: 5,
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleAdminUser)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/users/models?username=alice&start_timestamp=1000&end_timestamp=2000", nil)

	GetUserModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.True(t, payload.Success, payload.Message)
	require.Len(t, payload.Data, 2)
	// ordered by quota DESC: gpt-a (100+30) first, then gpt-b (50+10);
	// bob's rows (username=bob) must not leak into alice's aggregation
	assert.Equal(t, "gpt-a", payload.Data[0].ModelName)
	assert.Equal(t, 3, payload.Data[0].Count)
	assert.Equal(t, 130, payload.Data[0].Quota)
	assert.Equal(t, "gpt-b", payload.Data[1].ModelName)
	assert.Equal(t, 3, payload.Data[1].Count)
	assert.Equal(t, 60, payload.Data[1].Quota)
	assert.Equal(t, 25, payload.Data[1].TokenUsed)
}

func TestGetUserModelQuotaDataRejectsEmptyUsername(t *testing.T) {
	setupTokenModelControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleAdminUser)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/users/models?username=&start_timestamp=1000&end_timestamp=2000", nil)

	GetUserModelQuotaData(ctx)

	payload := decodeTokenModelQuotaResponse(t, recorder)
	require.False(t, payload.Success)
	assert.Equal(t, "invalid username", payload.Message)
}
