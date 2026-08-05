package controller

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConvertRealUCloudCSV 用真实 UCloud CSV 做端到端转换验证。
// 不设本地过滤（全部模型视为已存在），检查整体转换结果的正确性。
func TestConvertRealUCloudCSV(t *testing.T) {
	csvPath := `C:\Users\Administrator\Desktop\UCloud模型计费单元.csv`
	f, err := os.Open(csvPath)
	if err != nil {
		t.Skipf("真实 CSV 不存在，跳过: %v", err)
	}
	defer f.Close()

	ctx := context.Background()

	// 先解析出行收集全部模型名，作为 localModels（跳过过滤）
	rows, err := parseCSVRows(mustOpen(t, csvPath), ctx)
	require.NoError(t, err)
	require.Equal(t, 1443, len(rows), "应解析出全部 1443 行")

	localModels := make(map[string]bool)
	for _, r := range rows {
		if r.ModelID != "" {
			localModels[r.ModelID] = true
		}
	}
	require.Equal(t, 225, len(localModels), "应有 225 个唯一模型")

	converted, displayPrices, skipped, err := convertCSVToRatioData(mustOpen(t, csvPath), localModels, ctx)
	require.NoError(t, err)
	assertEmptySlice(t, skipped)

	// 不再有 model_ratio/completion_ratio 等倍率字段
	prices := converted["model_price"].(map[string]float64)
	exprs := converted[billing_setting.BillingExprField].(map[string]string)

	t.Logf("固定价模型: %d, 表达式模型: %d", len(prices), len(exprs))
	total := len(prices) + len(exprs)
	require.Greater(t, total, 200, "绝大多数模型应被成功转换")

	// 抽样校验：doubao 简单模型 → 表达式（不再走倍率）
	require.Contains(t, exprs, "ByteDance/doubao-1-5-pro-32k-250115")
	assert.Equal(t, "p*0.8 + c*2 + cr*0.16", exprs["ByteDance/doubao-1-5-pro-32k-250115"])

	// 抽样校验：MiniMax-M3 阶梯模型 → 表达式
	require.Contains(t, exprs, "MiniMax-M3")
	m3 := exprs["MiniMax-M3"]
	require.Contains(t, m3, "len <= 512000")
	require.Contains(t, m3, `tier("standard"`)
	require.Contains(t, m3, `tier("long_context"`)
	t.Logf("MiniMax-M3 表达式: %s", m3)

	// 抽样校验：Qwen3-Coder 三档阶梯 → 表达式
	require.Contains(t, exprs, "Qwen/Qwen3-Coder")
	coder := exprs["Qwen/Qwen3-Coder"]
	require.Contains(t, coder, "len <= 32000")
	require.Contains(t, coder, "len <= 128000")
	t.Logf("Qwen3-Coder 表达式: %s", coder)

	// 抽样校验：midjourney 按次 → 固定价
	require.Contains(t, prices, "midjourney-fast-imagine")
	assertFloat(t, 0.712, prices["midjourney-fast-imagine"])

	// 抽样校验：kling 视频按时长（代表价为最高档）→ 固定价
	require.Contains(t, prices, "kling-v3")

	// 免费模型 → 表达式零价
	require.Contains(t, exprs, "BAAI/bge-large-zh-v1.5")
	assert.Contains(t, exprs["BAAI/bge-large-zh-v1.5"], "p*0")

	// 抽样校验：可读价格文本
	require.Contains(t, displayPrices, "ByteDance/doubao-1-5-pro-32k-250115")
	assert.Equal(t, "Expression billing", displayPrices["ByteDance/doubao-1-5-pro-32k-250115"]["billing_mode"])
	doubaoDisp, ok := displayPrices["ByteDance/doubao-1-5-pro-32k-250115"]["billing_expr"].([]displayPriceLine)
	require.True(t, ok)
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "0.8 元/M"},
		{Label: "Output price", Value: "2 元/M"},
		{Label: "Cache read price", Value: "0.16 元/M"},
	}, doubaoDisp)
	require.Contains(t, displayPrices, "MiniMax-M3")
	assert.Equal(t, "Expression billing", displayPrices["MiniMax-M3"]["billing_mode"])
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "4.2/8.4 元/M"},
		{Label: "Output price", Value: "16.8/33.6 元/M"},
		{Label: "Cache read price", Value: "0.84/1.68 元/M"},
	}, displayPrices["MiniMax-M3"]["billing_expr"])
	require.Contains(t, displayPrices, "midjourney-fast-imagine")
	assert.Equal(t, "0.712 元/次", displayPrices["midjourney-fast-imagine"]["model_price"])

	// 表达式模型不应同时出现在价格 map
	for m := range exprs {
		require.NotContains(t, prices, m, "表达式模型 %s 不应有固定价", m)
	}

	// 打印前 10 个表达式模型名，人工核对
	exprModels := make([]string, 0, len(exprs))
	for m := range exprs {
		exprModels = append(exprModels, m)
	}
	sort.Strings(exprModels)
	if len(exprModels) > 10 {
		t.Logf("表达式模型示例(前10): %v ...", exprModels[:10])
	} else {
		t.Logf("表达式模型: %v", exprModels)
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

func assertFloat(t *testing.T, want, got float64) {
	t.Helper()
	require.InDelta(t, want, got, 1e-6, "want %v, got %v", want, got)
}

func assertEmptySlice(t *testing.T, s []string) {
	t.Helper()
	require.Empty(t, s)
}
