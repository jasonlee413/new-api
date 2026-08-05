package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDesc(t *testing.T) {
	tests := []struct {
		desc     string
		wantType string
		wantCond string
	}{
		{"输入 - 默认", "输入", "默认"},
		{"文本输出 - 默认", "文本输出", "默认"},
		{"输入 - 输入长度(0, 512K]", "输入", "输入长度(0, 512K]"},
		{"缓存写入 (5m) - 默认", "缓存写入 (5m)", "默认"},
		{"视频时长 - 1080p 且 duration=6", "视频时长", "1080p 且 duration=6"},
		{"无分隔符", "无分隔符", ""},
	}
	for _, tt := range tests {
		typ, cond := parseDesc(tt.desc)
		assert.Equal(t, tt.wantType, typ, "desc=%q", tt.desc)
		assert.Equal(t, tt.wantCond, cond, "desc=%q", tt.desc)
	}
}

func TestParseTierUpperBound(t *testing.T) {
	tests := []struct {
		cond string
		want int64
	}{
		{"输入长度(0, 512K]", 512000},
		{"输入长度(512K, 1M]", 1000000},
		{"输入长度(0, 32K]", 32000},
		{"输入长度(32K, 128K]", 128000},
		{"输入长度(128K, 256K]", 256000},
		{"默认", 0},
		{"1080p 且 duration=6", 0},
		{"", 0},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, parseTierUpperBound(tt.cond), "cond=%q", tt.cond)
	}
}

func TestFormatPrice(t *testing.T) {
	assert.Equal(t, "4.2", formatPrice(4.2))
	assert.Equal(t, "0.0432", formatPrice(0.0432))
	assert.Equal(t, "0", formatPrice(0))
	assert.Equal(t, "16.8", formatPrice(16.8))
	assert.Equal(t, "0.3", formatPrice(0.1+0.2)) // 避免浮点尾巴
}

func TestParseCSVRows(t *testing.T) {
	ctx := context.Background()

	t.Run("标准解析含BOM", func(t *testing.T) {
		csvData := "\xEF\xBB\xBF\"模型ID\",\"计费单元\",\"说明\",\"标价(元)\",\"单位\"\n" +
			"\"model-a\",\"unit-a\",\"输入 - 默认\",\"0.8\",\"百万 Token\"\n" +
			"\"model-a\",\"unit-b\",\"文本输出 - 默认\",\"2\",\"百万 Token\"\n"
		rows, err := parseCSVRows(strings.NewReader(csvData), ctx)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		assert.Equal(t, "model-a", rows[0].ModelID)
		assert.Equal(t, "输入 - 默认", rows[0].Desc)
		assert.InDelta(t, 0.8, rows[0].Price, 1e-9)
		assert.Equal(t, "百万 Token", rows[0].Unit)
	})

	t.Run("跳过无效价格行", func(t *testing.T) {
		csvData := "\"模型ID\",\"计费单元\",\"说明\",\"标价(元)\",\"单位\"\n" +
			"\"model-a\",\"u\",\"输入 - 默认\",\"abc\",\"百万 Token\"\n" +
			"\"model-b\",\"u\",\"输入 - 默认\",\"-1\",\"百万 Token\"\n" +
			"\"model-c\",\"u\",\"输入 - 默认\",\"1.5\",\"百万 Token\"\n"
		rows, err := parseCSVRows(strings.NewReader(csvData), ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "model-c", rows[0].ModelID)
	})

	t.Run("表头列数不足报错", func(t *testing.T) {
		_, err := parseCSVRows(strings.NewReader("\"a\",\"b\"\n"), ctx)
		require.Error(t, err)
	})
}

func TestBuildTieredExpression(t *testing.T) {
	t.Run("单档无阶梯", func(t *testing.T) {
		tiers := map[string][]priceTier{
			"p":  {{UpperBound: 0, Price: 0.8}},
			"c":  {{UpperBound: 0, Price: 2}},
			"cr": {{UpperBound: 0, Price: 0.16}},
		}
		expr := buildTieredExpression(tiers)
		assert.Equal(t, "p*0.8 + c*2 + cr*0.16", expr)
	})

	t.Run("两档阶梯", func(t *testing.T) {
		tiers := map[string][]priceTier{
			"p":  {{UpperBound: 512000, Price: 4.2}, {UpperBound: 1000000, Price: 8.4}},
			"c":  {{UpperBound: 512000, Price: 16.8}, {UpperBound: 1000000, Price: 33.6}},
			"cr": {{UpperBound: 512000, Price: 0.84}, {UpperBound: 1000000, Price: 1.68}},
		}
		expr := buildTieredExpression(tiers)
		expected := `len <= 512000 ? tier("standard", p*4.2 + c*16.8 + cr*0.84) : (tier("long_context", p*8.4 + c*33.6 + cr*1.68))`
		assert.Equal(t, expected, expr)
	})

	t.Run("三档阶梯", func(t *testing.T) {
		tiers := map[string][]priceTier{
			"p": {{UpperBound: 32000, Price: 6}, {UpperBound: 128000, Price: 9}, {UpperBound: 256000, Price: 15}},
			"c": {{UpperBound: 32000, Price: 24}, {UpperBound: 128000, Price: 36}, {UpperBound: 256000, Price: 60}},
		}
		expr := buildTieredExpression(tiers)
		expected := `len <= 32000 ? tier("tier_1", p*6 + c*24) : (len <= 128000 ? tier("tier_2", p*9 + c*36) : (tier("tier_3", p*15 + c*60)))`
		assert.Equal(t, expected, expr)
	})

	t.Run("多模态变量", func(t *testing.T) {
		tiers := map[string][]priceTier{
			"p":     {{UpperBound: 0, Price: 0.3}},
			"c":     {{UpperBound: 0, Price: 2.5}},
			"img":   {{UpperBound: 0, Price: 0.3}},
			"img_o": {{UpperBound: 0, Price: 2.5}},
		}
		expr := buildTieredExpression(tiers)
		assert.Equal(t, "p*0.3 + c*2.5 + img*0.3 + img_o*2.5", expr)
	})

	t.Run("阶梯缺失变量回退默认档", func(t *testing.T) {
		// p 有阶梯，cr 只有默认档：各档位均使用 cr 默认价
		tiers := map[string][]priceTier{
			"p":  {{UpperBound: 512000, Price: 4.2}, {UpperBound: 1000000, Price: 8.4}},
			"cr": {{UpperBound: 0, Price: 0.84}},
		}
		expr := buildTieredExpression(tiers)
		expected := `len <= 512000 ? tier("standard", p*4.2 + cr*0.84) : (tier("long_context", p*8.4 + cr*0.84))`
		assert.Equal(t, expected, expr)
	})
}

func TestSelectRepresentativePrice(t *testing.T) {
	t.Run("默认档优先", func(t *testing.T) {
		candidates := []csvPriceRow{
			{Desc: "视频时长 - 720p", Price: 0.3},
			{Desc: "视频时长 - 默认", Price: 0.5},
			{Desc: "视频时长 - 1080p", Price: 0.58},
		}
		assert.InDelta(t, 0.5, selectRepresentativePrice(candidates), 1e-9)
	})

	t.Run("无默认取最高价", func(t *testing.T) {
		candidates := []csvPriceRow{
			{Desc: "视频时长 - 768p 且 duration=6", Price: 0.3333},
			{Desc: "视频时长 - 1080p 且 duration=6", Price: 0.5833},
		}
		assert.InDelta(t, 0.5833, selectRepresentativePrice(candidates), 1e-9)
	})
}

func TestBuildTokenPricing(t *testing.T) {
	ctx := context.Background()

	t.Run("简单模型出表达式", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 默认", Price: 0.8, Unit: "百万 Token"},
			{Desc: "文本输出 - 默认", Price: 2, Unit: "百万 Token"},
			{Desc: "缓存 - 默认", Price: 0.16, Unit: "百万 Token"},
		}
		result := newCSVConvertResult()
		buildTokenPricing("model-a", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["model-a"])
		assert.Equal(t, "p*0.8 + c*2 + cr*0.16", result.billingExprs["model-a"])
		assert.Empty(t, result.modelPrices)
	})

	t.Run("免费模型出表达式零价", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 默认", Price: 0, Unit: "百万 Token"},
			{Desc: "文本输出 - 默认", Price: 0, Unit: "百万 Token"},
		}
		result := newCSVConvertResult()
		buildTokenPricing("free-model", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["free-model"])
		assert.Equal(t, "p*0 + c*0", result.billingExprs["free-model"])
	})

	t.Run("阶梯模型出表达式", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 输入长度(0, 512K]", Price: 4.2, Unit: "百万 Token"},
			{Desc: "输入 - 输入长度(512K, 1M]", Price: 8.4, Unit: "百万 Token"},
			{Desc: "文本输出 - 输入长度(0, 512K]", Price: 16.8, Unit: "百万 Token"},
			{Desc: "文本输出 - 输入长度(512K, 1M]", Price: 33.6, Unit: "百万 Token"},
		}
		result := newCSVConvertResult()
		buildTokenPricing("tiered-model", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["tiered-model"])
		assert.Contains(t, result.billingExprs["tiered-model"], "len <= 512000")
		assert.Contains(t, result.billingExprs["tiered-model"], `tier("standard"`)
	})

	t.Run("多模态模型出表达式", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 默认", Price: 0.3, Unit: "百万 Token"},
			{Desc: "文本输出 - 默认", Price: 2.5, Unit: "百万 Token"},
			{Desc: "图像输入 - 默认", Price: 0.3, Unit: "百万 Token"},
			{Desc: "图像输出 - 默认", Price: 2.5, Unit: "百万 Token"},
		}
		result := newCSVConvertResult()
		buildTokenPricing("mm-model", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["mm-model"])
		assert.Contains(t, result.billingExprs["mm-model"], "img*0.3")
		assert.Contains(t, result.billingExprs["mm-model"], "img_o*2.5")
	})

	t.Run("含cc1h模型出表达式", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 默认", Price: 2.1, Unit: "百万 Token"},
			{Desc: "文本输出 - 默认", Price: 8.4, Unit: "百万 Token"},
			{Desc: "缓存读取 - 默认", Price: 0.21, Unit: "百万 Token"},
			{Desc: "缓存写入 (5m) - 默认", Price: 2.625, Unit: "百万 Token"},
			{Desc: "缓存写入 (1h) - 默认", Price: 4.2, Unit: "百万 Token"},
		}
		result := newCSVConvertResult()
		buildTokenPricing("cc1h-model", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["cc1h-model"])
		assert.Contains(t, result.billingExprs["cc1h-model"], "cc1h*4.2")
		assert.Contains(t, result.billingExprs["cc1h-model"], "cc*2.625")
	})

	t.Run("跳过缓存存储类型出表达式", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "输入 - 默认", Price: 0.15, Unit: "百万 Token"},
			{Desc: "文本输出 - 默认", Price: 0.6, Unit: "百万 Token"},
			{Desc: "缓存存储 (1h) - 默认", Price: 0.0000072, Unit: ""},
		}
		result := newCSVConvertResult()
		// 缓存存储行单位为空，在 buildModelPricing 层已被过滤；
		// 这里直接测试 buildTokenPricing 收到该行时也能正确跳过
		buildTokenPricing("gemini-model", rows, result, ctx)
		assert.Equal(t, "tiered_expr", result.billingModes["gemini-model"])
		assert.Contains(t, result.billingExprs["gemini-model"], "p*0.15")
		assert.Contains(t, result.billingExprs["gemini-model"], "c*0.6")
	})
}

func TestBuildFixedPricing(t *testing.T) {
	t.Run("单位优先级秒高于次", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "音频数量 - 默认", Price: 0.46875, Unit: "次"},
			{Desc: "视频时长 - 1080p 且 duration=6", Price: 0.5833, Unit: "秒"},
		}
		result := newCSVConvertResult()
		buildFixedPricing("vidu-model", rows, result)
		assert.InDelta(t, 0.5833, result.modelPrices["vidu-model"], 1e-9)
	})

	t.Run("请求次数固定价", func(t *testing.T) {
		rows := []csvPriceRow{
			{Desc: "请求次数 - 默认", Price: 0.712, Unit: "次"},
		}
		result := newCSVConvertResult()
		buildFixedPricing("mj-model", rows, result)
		assert.InDelta(t, 0.712, result.modelPrices["mj-model"], 1e-9)
	})
}

func TestConvertCSVToRatioData(t *testing.T) {
	ctx := context.Background()
	csvData := "\xEF\xBB\xBF\"模型ID\",\"计费单元\",\"说明\",\"标价(元)\",\"单位\"\n" +
		"\"local-model\",\"u1\",\"输入 - 默认\",\"0.8\",\"百万 Token\"\n" +
		"\"local-model\",\"u2\",\"文本输出 - 默认\",\"2\",\"百万 Token\"\n" +
		"\"remote-model\",\"u3\",\"输入 - 默认\",\"1\",\"百万 Token\"\n" +
		"\"remote-model\",\"u4\",\"文本输出 - 默认\",\"4\",\"百万 Token\"\n" +
		"\"fixed-model\",\"u5\",\"请求次数 - 默认\",\"0.712\",\"次\"\n"

	localModels := map[string]bool{
		"local-model": true,
		"fixed-model": true,
	}

	converted, displayPrices, skipped, err := convertCSVToRatioData(strings.NewReader(csvData), localModels, ctx)
	require.NoError(t, err)

	// remote-model 不在本地，应被跳过
	assert.Equal(t, []string{"remote-model"}, skipped)

	// local-model 出表达式（不再出倍率）
	billingModes, ok := converted["billing_setting.billing_mode"].(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "tiered_expr", billingModes["local-model"])
	_, hasRemote := billingModes["remote-model"]
	assert.False(t, hasRemote)

	exprs, ok := converted["billing_setting.billing_expr"].(map[string]string)
	require.True(t, ok)
	assert.Contains(t, exprs["local-model"], "p*0.8")
	assert.Contains(t, exprs["local-model"], "c*2")

	// 确认不再产出旧 model_ratio/completion_ratio 等倍率字段
	_, hasModelRatio := converted["model_ratio"]
	assert.False(t, hasModelRatio)
	_, hasCompletionRatio := converted["completion_ratio"]
	assert.False(t, hasCompletionRatio)
	_, hasCacheRatio := converted["cache_ratio"]
	assert.False(t, hasCacheRatio)
	_, hasCreateCacheRatio := converted["create_cache_ratio"]
	assert.False(t, hasCreateCacheRatio)

	// fixed-model 出固定价
	prices, ok := converted["model_price"].(map[string]float64)
	require.True(t, ok)
	assert.InDelta(t, 0.712, prices["fixed-model"], 1e-9)

	// 可读价格文本：表达式模型展示 Expression billing + 变量价格
	assert.Equal(t, "Expression billing", displayPrices["local-model"]["billing_mode"])
	dispLines, ok := displayPrices["local-model"]["billing_expr"].([]displayPriceLine)
	require.True(t, ok)
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "0.8 元/M"},
		{Label: "Output price", Value: "2 元/M"},
	}, dispLines)
	// 固定价模型带单位
	assert.Equal(t, "0.712 元/次", displayPrices["fixed-model"]["model_price"])
	// 被跳过模型无展示文本
	_, hasRemoteDisplay := displayPrices["remote-model"]
	assert.False(t, hasRemoteDisplay)
}

func TestConvertCSVToRatioDataEmpty(t *testing.T) {
	ctx := context.Background()
	_, _, _, err := convertCSVToRatioData(strings.NewReader("\"a\",\"b\",\"c\",\"d\",\"e\"\n"), map[string]bool{}, ctx)
	require.Error(t, err)
}

func TestBuildExprDisplayLines(t *testing.T) {
	// 单档：每个变量一条价格行
	single := map[string][]priceTier{
		"p":  {{Price: 4.2}},
		"c":  {{Price: 16.8}},
		"cr": {{Price: 0.84}},
	}
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "4.2 元/M"},
		{Label: "Output price", Value: "16.8 元/M"},
		{Label: "Cache read price", Value: "0.84 元/M"},
	}, buildExprDisplayLines(single))

	// 阶梯：多档价格按档位升序以 / 分隔（每档均有明确上限，如 (512K, 1M] → 1000000）
	tiered := map[string][]priceTier{
		"p": {{UpperBound: 512000, Price: 4.2}, {UpperBound: 1000000, Price: 8.4}},
		"c": {{UpperBound: 512000, Price: 16.8}, {UpperBound: 1000000, Price: 33.6}},
	}
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "4.2/8.4 元/M"},
		{Label: "Output price", Value: "16.8/33.6 元/M"},
	}, buildExprDisplayLines(tiered))

	// 同价档位合并：各档价格相同只显示一个值
	samePriced := map[string][]priceTier{
		"p": {{UpperBound: 512000, Price: 35.6}, {UpperBound: 1000000, Price: 35.6}},
	}
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "35.6 元/M"},
	}, buildExprDisplayLines(samePriced))

	// 多模态变量按固定顺序输出
	multimodal := map[string][]priceTier{
		"img": {{Price: 1.5}},
		"p":   {{Price: 2}},
		"c":   {{Price: 8}},
	}
	assert.Equal(t, []displayPriceLine{
		{Label: "Input price", Value: "2 元/M"},
		{Label: "Output price", Value: "8 元/M"},
		{Label: "Image input price", Value: "1.5 元/M"},
	}, buildExprDisplayLines(multimodal))
}
