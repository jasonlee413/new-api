package controller

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/gin-gonic/gin"
)

const (
	csvImportChannelName = "CSV 文件导入"
	maxCSVFileSize       = 10 << 20 // 10MB
)

// csvPriceRow 一行 CSV 计费数据的解析结果
type csvPriceRow struct {
	ModelID string  // 模型ID
	Desc    string  // 说明（"类型 - 条件"）
	Price   float64 // 标价（元）
	Unit    string  // 单位（百万 Token/秒/次/张/分/页/...）
}

// priceTier 一个计费变量的单档价格
type priceTier struct {
	UpperBound int64   // 档位上限（tokens），0 表示无阶梯（默认档）
	Price      float64 // 元/百万 tokens
}

// csvDescTypeToVar 说明类型（" - "前缀）→ 计费变量
var csvDescTypeToVar = map[string]string{
	"输入":       "p",
	"文本输入":     "p",
	"文本输出":     "c",
	"缓存":       "cr",
	"缓存读取":     "cr",
	"缓存写入 (5m)": "cc",
	"缓存写入":     "cc",
	"缓存写入 (1h)": "cc1h",
	"图像输入":     "img",
	"图像输出":     "img_o",
	"音频输入":     "ai",
	"音频输出":     "ao",
	"图像缓存读取":   "cr",
	"视频缓存读取":   "cr",
	"音频缓存读取":   "cr",
	"视频输入":     "p",
	"视频输出":     "c",
	"文档输入":     "p",
}

// csvDescTypeSkip 无法映射、直接跳过的说明类型
var csvDescTypeSkip = map[string]bool{
	"缓存存储 (1h)": true, // new-api 不支持缓存存储按时计费
}

// fixedUnitPriority 非 token 类单位优先级（同模型多单位时取优先级最高者）
var fixedUnitPriority = []string{"秒", "次", "张", "分", "页", "百万像素", "字符"}

// exprVarOrder 表达式中变量的固定输出顺序，保证生成结果稳定
var exprVarOrder = []string{"p", "c", "cr", "cc", "cc1h", "img", "img_o", "ai", "ao"}

// exprVarDisplayLabel 计费变量 → 前端 i18n 标签 key（用于差异对比中的可读价格行）
var exprVarDisplayLabel = map[string]string{
	"p":     "Input price",
	"c":     "Output price",
	"cr":    "Cache read price",
	"cc":    "Cache write price",
	"cc1h":  "Cache Write (1h)",
	"img":   "Image input price",
	"img_o": "Image output price",
	"ai":    "Audio input price",
	"ao":    "Audio output price",
}

// displayPriceLine 一条可读价格行：label 为前端 i18n key，value 为价格文本（如 "4.2/8.4 元/M"）
type displayPriceLine struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var tierBoundRegex = regexp.MustCompile(`输入长度\([^,]+,\s*([0-9.]+)\s*([KkMm])?\s*\]`)

// parseDesc 将"说明"列拆分为类型与条件，如 "输入 - 输入长度(0, 512K]" → ("输入", "输入长度(0, 512K]")
func parseDesc(desc string) (typ, cond string) {
	parts := strings.SplitN(desc, " - ", 2)
	typ = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		cond = strings.TrimSpace(parts[1])
	}
	return
}

// parseTierUpperBound 从条件中提取阶梯上限，"输入长度(0, 512K]" → 512000；无阶梯返回 0
func parseTierUpperBound(cond string) int64 {
	m := tierBoundRegex.FindStringSubmatch(cond)
	if m == nil {
		return 0
	}
	num, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(m[2]) {
	case "K":
		num *= 1000
	case "M":
		num *= 1000000
	}
	return int64(num)
}

// formatPrice 输出最简十进制价格字符串，避免浮点尾巴（如 0.30000000000000004）
func formatPrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}

// parseCSVRows 解析 CSV 全部数据行，自动跳过 UTF-8 BOM 与表头
func parseCSVRows(reader io.Reader, ctx context.Context) ([]csvPriceRow, error) {
	br := bufio.NewReader(reader)
	if bom, err := br.Peek(3); err == nil && len(bom) == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = br.Discard(3)
	}

	r := csv.NewReader(br)
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("读取 CSV 表头失败: %w", err)
	}
	if len(header) < 5 {
		return nil, fmt.Errorf("CSV 表头列数不足: 期望至少 5 列，实际 %d 列", len(header))
	}

	var rows []csvPriceRow
	lineNum := 1
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		lineNum++
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行解析失败: %v", lineNum, err))
			continue
		}
		if len(record) < 5 {
			continue
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(record[3]), 64)
		if err != nil || !isValidNonNegativeCost(price) {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行价格无效: %q", lineNum, record[3]))
			continue
		}
		rows = append(rows, csvPriceRow{
			ModelID: strings.TrimSpace(record[0]),
			Desc:    strings.TrimSpace(record[2]),
			Price:   price,
			Unit:    strings.TrimSpace(record[4]),
		})
	}
	return rows, nil
}

// csvConvertResult 转换结果的分类收集器
type csvConvertResult struct {
	modelPrices  map[string]float64
	billingModes map[string]string
	billingExprs map[string]string
	// displayPrices 每个模型各同步字段的可读价格展示内容，供差异对比界面直接展示：
	// 简单字段为 string（如 "4.2 元/M"），billing_expr 为 []displayPriceLine（逐变量价格行）
	displayPrices map[string]map[string]any
}

func newCSVConvertResult() *csvConvertResult {
	return &csvConvertResult{
		modelPrices:  make(map[string]float64),
		billingModes: make(map[string]string),
		billingExprs: make(map[string]string),
		displayPrices: make(map[string]map[string]any),
	}
}

// setDisplayPrice 记录某模型某字段的可读价格展示内容
func (r *csvConvertResult) setDisplayPrice(modelID, field string, content any) {
	dp, ok := r.displayPrices[modelID]
	if !ok {
		dp = make(map[string]any)
		r.displayPrices[modelID] = dp
	}
	dp[field] = content
}

// singlePrice 取变量单档价格（无阶梯场景）
func singlePrice(list []priceTier) float64 {
	if len(list) == 0 {
		return 0
	}
	return list[0].Price
}

// collectTierBounds 收集所有变量的阶梯上限，去重升序
func collectTierBounds(tiers map[string][]priceTier) []int64 {
	boundSet := make(map[int64]bool)
	for _, list := range tiers {
		for _, t := range list {
			if t.UpperBound > 0 {
				boundSet[t.UpperBound] = true
			}
		}
	}
	bounds := make([]int64, 0, len(boundSet))
	for b := range boundSet {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	return bounds
}

// priceForBound 取变量在指定档位的价格：优先精确匹配档位上限，其次回退默认档
func priceForBound(list []priceTier, bound int64) (float64, bool) {
	for _, t := range list {
		if t.UpperBound == bound {
			return t.Price, true
		}
	}
	for _, t := range list {
		if t.UpperBound == 0 {
			return t.Price, true
		}
	}
	return 0, false
}

// buildExprSum 生成某一档位的表达式求和部分，如 "p*4.2 + c*16.8 + cr*0.84"
// bound 为 0 表示单档（取各变量默认档或唯一档价格）
func buildExprSum(tiers map[string][]priceTier, bound int64) string {
	parts := make([]string, 0, len(tiers))
	for _, v := range exprVarOrder {
		list, ok := tiers[v]
		if !ok {
			continue
		}
		var price float64
		var found bool
		if bound == 0 {
			price, found = priceForBound(list, 0)
			if !found && len(list) == 1 {
				price, found = list[0].Price, true
			}
		} else {
			price, found = priceForBound(list, bound)
		}
		if found {
			parts = append(parts, fmt.Sprintf("%s*%s", v, formatPrice(price)))
		}
	}
	return strings.Join(parts, " + ")
}

// tierNames 档位命名：两档用语义名，三档及以上用序号
func tierNames(n int) []string {
	if n == 2 {
		return []string{"standard", "long_context"}
	}
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("tier_%d", i+1)
	}
	return names
}

// buildTieredExpression 由按变量归类的档位价格生成完整计费表达式。
// 无阶梯时输出简单求和；有阶梯时生成嵌套三元，末档为 else 分支。
func buildTieredExpression(tiers map[string][]priceTier) string {
	bounds := collectTierBounds(tiers)
	if len(bounds) == 0 {
		return buildExprSum(tiers, 0)
	}
	names := tierNames(len(bounds))
	last := len(bounds) - 1
	expr := fmt.Sprintf("tier(%q, %s)", names[last], buildExprSum(tiers, bounds[last]))
	for i := last - 1; i >= 0; i-- {
		expr = fmt.Sprintf("len <= %d ? tier(%q, %s) : (%s)", bounds[i], names[i], buildExprSum(tiers, bounds[i]), expr)
	}
	return expr
}

// buildExprDisplayLines 由按变量归类的档位价格生成结构化可读价格行，每个变量一条，
// 如 {label: "Input price", value: "4.2/8.4 元/M"}（多档价格按档位升序以 / 分隔，同价档位合并）
func buildExprDisplayLines(tiers map[string][]priceTier) []displayPriceLine {
	bounds := collectTierBounds(tiers)
	lines := make([]displayPriceLine, 0, len(tiers))
	for _, v := range exprVarOrder {
		list, ok := tiers[v]
		if !ok {
			continue
		}
		label := exprVarDisplayLabel[v]
		if len(bounds) == 0 {
			lines = append(lines, displayPriceLine{
				Label: label,
				Value: fmt.Sprintf("%s 元/M", formatPrice(singlePrice(list))),
			})
			continue
		}
		prices := make([]string, 0, len(bounds))
		seen := make(map[string]bool)
		for _, b := range bounds {
			if p, found := priceForBound(list, b); found {
				s := formatPrice(p)
				if !seen[s] {
					seen[s] = true
					prices = append(prices, s)
				}
			}
		}
		lines = append(lines, displayPriceLine{
			Label: label,
			Value: fmt.Sprintf("%s 元/M", strings.Join(prices, "/")),
		})
	}
	return lines
}

// buildTokenPricing 处理 token 类模型：简单模型出倍率，阶梯/多模态/含 cc1h 模型出表达式
func buildTokenPricing(modelID string, rows []csvPriceRow, result *csvConvertResult, ctx context.Context) {
	tiers := make(map[string][]priceTier)
	for _, r := range rows {
		typ, cond := parseDesc(r.Desc)
		if csvDescTypeSkip[typ] {
			continue
		}
		varName, ok := csvDescTypeToVar[typ]
		if !ok {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 存在未识别的说明类型: %q", modelID, typ))
			continue
		}
		bound := parseTierUpperBound(cond)
		tiers[varName] = append(tiers[varName], priceTier{UpperBound: bound, Price: r.Price})
	}

	if len(tiers) == 0 {
		return
	}

	// 统一走直接单价表达式：p*输入价 + c*输出价 + cr*缓存价 + cc*写缓存价
	// 表达式系数即 CSV 人民币价格，不再走 model_ratio/completion_ratio 倍率换算
	expr := buildTieredExpression(tiers)
	if _, err := billingexpr.CompileFromCache(expr); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("模型 %s 表达式编译失败，已跳过: %v", modelID, err))
		return
	}
	if err := billing_setting.SmokeTestExpr(expr); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("模型 %s 表达式冒烟测试失败，已跳过: %v", modelID, err))
		return
	}
	result.billingModes[modelID] = billing_setting.BillingModeTieredExpr
	result.billingExprs[modelID] = expr
	result.setDisplayPrice(modelID, "billing_mode", "Expression billing")
	result.setDisplayPrice(modelID, "billing_expr", buildExprDisplayLines(tiers))
}

// selectRepresentativePrice 代表价选择：默认档优先，无默认取最高价（保守防亏损）
func selectRepresentativePrice(candidates []csvPriceRow) float64 {
	for _, c := range candidates {
		_, cond := parseDesc(c.Desc)
		if cond == "默认" {
			return c.Price
		}
	}
	maxPrice := 0.0
	for _, c := range candidates {
		if c.Price > maxPrice {
			maxPrice = c.Price
		}
	}
	return maxPrice
}

// buildFixedPricing 处理非 token 类模型：按单位优先级选代表价，生成 model_price
func buildFixedPricing(modelID string, rows []csvPriceRow, result *csvConvertResult) {
	byUnit := make(map[string][]csvPriceRow)
	for _, r := range rows {
		byUnit[r.Unit] = append(byUnit[r.Unit], r)
	}
	for _, unit := range fixedUnitPriority {
		candidates, ok := byUnit[unit]
		if !ok {
			continue
		}
		price := selectRepresentativePrice(candidates)
		result.modelPrices[modelID] = roundRatioValue(price)
		result.setDisplayPrice(modelID, "model_price", fmt.Sprintf("%s 元/%s", formatPrice(price), unit))
		return
	}
}

// buildModelPricing 按模型分发：token 类优先，纯非 token 类走固定价
func buildModelPricing(modelID string, rows []csvPriceRow, result *csvConvertResult, ctx context.Context) {
	var tokenRows, fixedRows []csvPriceRow
	for _, r := range rows {
		switch {
		case r.Unit == "百万 Token":
			tokenRows = append(tokenRows, r)
		case r.Unit == "":
			// 空单位（gemini 缓存存储等）跳过
		default:
			fixedRows = append(fixedRows, r)
		}
	}

	if len(tokenRows) > 0 {
		buildTokenPricing(modelID, tokenRows, result, ctx)
	} else if len(fixedRows) > 0 {
		buildFixedPricing(modelID, fixedRows, result)
	}
}

// convertCSVToRatioData 将 CSV 内容转换为同步数据格式，仅保留 localModels 中存在的模型。
// 返回的 map key 与 pricingSyncFields 对齐，可直接交给 buildDifferences 复用；
// 同时返回每个模型各字段的可读价格展示内容，供差异对比界面展示。
func convertCSVToRatioData(reader io.Reader, localModels map[string]bool, ctx context.Context) (map[string]any, map[string]map[string]any, []string, error) {
	rows, err := parseCSVRows(reader, ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil, fmt.Errorf("CSV 中无有效数据行")
	}

	modelRows := make(map[string][]csvPriceRow)
	for _, r := range rows {
		if r.ModelID == "" {
			continue
		}
		modelRows[r.ModelID] = append(modelRows[r.ModelID], r)
	}

	result := newCSVConvertResult()
	var skippedModels []string

	for modelID, mRows := range modelRows {
		if !localModels[modelID] {
			skippedModels = append(skippedModels, modelID)
			continue
		}
		buildModelPricing(modelID, mRows, result, ctx)
	}
	sort.Strings(skippedModels)

	converted := make(map[string]any)
	if len(result.modelPrices) > 0 {
		converted["model_price"] = result.modelPrices
	}
	if len(result.billingModes) > 0 {
		converted[billing_setting.BillingModeField] = result.billingModes
	}
	if len(result.billingExprs) > 0 {
		converted[billing_setting.BillingExprField] = result.billingExprs
	}
	return converted, result.displayPrices, skippedModels, nil
}

// FetchCSVUpstreamRatios 接收上传的 CSV 文件，解析后复用差异对比流程返回结果
func FetchCSVUpstreamRatios(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "请选择要上传的 CSV 文件"})
		return
	}
	if fileHeader.Size > maxCSVFileSize {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "CSV 文件大小不能超过 10MB"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".csv") {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "仅支持 .csv 文件"})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		logger.LogError(c.Request.Context(), "打开上传文件失败: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "无法读取上传文件"})
		return
	}
	defer file.Close()

	localData := getLocalPricingSyncData()
	// 匹配基准 = 启用渠道中的模型 ∪ 本地已配置价格的模型。
	// 本地未配置价格但渠道中存在的模型允许导入（作为新增定价），
	// 只有渠道中完全不存在的模型才会被跳过。
	localModels := make(map[string]bool)
	for _, field := range pricingSyncFields {
		for name := range valueMap(localData[field]) {
			localModels[name] = true
		}
	}
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		logger.LogError(c.Request.Context(), "查询渠道列表失败，仅按本地定价配置匹配: "+err.Error())
	} else {
		for _, channel := range channels {
			if channel.Status != common.ChannelStatusEnabled {
				continue
			}
			for _, m := range channel.GetModels() {
				if m = strings.TrimSpace(m); m != "" {
					localModels[m] = true
				}
			}
		}
	}

	converted, displayPrices, skippedModels, err := convertCSVToRatioData(file, localModels, c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "CSV 解析失败: " + err.Error()})
		return
	}

	successfulChannels := []struct {
		name string
		data map[string]any
	}{{name: csvImportChannelName, data: converted}}
	differences := buildDifferences(localData, successfulChannels)

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("CSV 定价导入解析完成: 跳过 %d 个渠道中不存在的模型", len(skippedModels)))

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"differences": differences,
			"test_results": []dto.TestResult{{
				Name:   csvImportChannelName,
				Status: "success",
			}},
			"skipped_models":  skippedModels,
			"display_prices": displayPrices,
		},
	})
}
