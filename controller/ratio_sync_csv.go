package controller

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
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
	"github.com/xuri/excelize/v2"
)

const (
	csvImportChannelName = "CSV/Excel 文件导入"
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

// parseDesc 将"说明"列拆分为类型与条件，如 "输入 - 输入长度(0, 512K]" → ("输入", "输入长度(0, 512K]")。
// 优先按 " - "（带空格）拆分；拆出的类型无法识别时，回退为已知类型最长前缀匹配，
// 以兼容 "输入-峰时 09:00-12:00（北京时间）" 这类无空格分隔的格式。
func parseDesc(desc string) (typ, cond string) {
	desc = strings.TrimSpace(desc)
	legacyTyp := desc
	legacyCond := ""
	if parts := strings.SplitN(desc, " - ", 2); len(parts) == 2 {
		legacyTyp = strings.TrimSpace(parts[0])
		legacyCond = strings.TrimSpace(parts[1])
	}
	if _, ok := csvDescTypeToVar[legacyTyp]; ok {
		return legacyTyp, legacyCond
	}
	if csvDescTypeSkip[legacyTyp] {
		return legacyTyp, legacyCond
	}
	// 最长前缀匹配：要求前缀后紧跟分隔符（-、–、— 或空白），避免 "输入输出比" 误匹配 "输入"
	best := ""
	for key := range csvDescTypeToVar {
		if len(key) > len(best) && strings.HasPrefix(desc, key) {
			best = key
		}
	}
	for key := range csvDescTypeSkip {
		if len(key) > len(best) && strings.HasPrefix(desc, key) {
			best = key
		}
	}
	if best == "" {
		return legacyTyp, legacyCond
	}
	rest := desc[len(best):]
	if rest != "" && !strings.HasPrefix(rest, "-") && !strings.HasPrefix(rest, "–") &&
		!strings.HasPrefix(rest, "—") && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return legacyTyp, legacyCond
	}
	cond = strings.TrimSpace(strings.TrimLeft(rest, "-–—"))
	return best, cond
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

// ---------------------------------------------------------------------------
// 峰谷时段条件解析
// ---------------------------------------------------------------------------

// minuteRange 一天内的分钟数区间 [Start, End)，End <= Start 表示跨午夜（如 18:00-次日08:00）
type minuteRange struct {
	Start int
	End   int
}

// timePeriodCond 一个计费时段条件：标签 + 时区 + 若干分钟数区间
type timePeriodCond struct {
	Label    string // 时段标签（如 "峰时"/"谷时"），用作 tier label
	Timezone string // IANA 时区（如 "Asia/Shanghai"）
	Ranges   []minuteRange
}

var timeRangeRegex = regexp.MustCompile(`(\d{1,2}):(\d{2})\s*-\s*(次日)?(\d{1,2}):(\d{2})`)
var tzTextRegex = regexp.MustCompile(`[（(]([^（）()]*)[）)]`)

// csvTimezoneAliases 说明文字中的时区别名 → IANA 时区
var csvTimezoneAliases = map[string]string{
	"北京时间":  "Asia/Shanghai",
	"北京":    "Asia/Shanghai",
	"中国标准时间": "Asia/Shanghai",
	"Asia/Shanghai": "Asia/Shanghai",
	"UTC+8":   "Asia/Shanghai",
	"utc+8":   "Asia/Shanghai",
}

// parseTimePeriodCondition 从条件文字解析时段条件，
// 如 "峰时 09:00-12:00、14:00-18:00（北京时间）"。
// 非时段条件（无时间范围）返回 (nil, nil)；结构类似但内容非法时返回错误。
func parseTimePeriodCondition(cond string) (*timePeriodCond, error) {
	if cond == "" || cond == "默认" {
		return nil, nil
	}
	rest := cond
	tz := "Asia/Shanghai" // 未标注时区时默认北京时间
	if m := tzTextRegex.FindStringSubmatch(rest); m != nil {
		text := strings.TrimSpace(m[1])
		mapped, ok := csvTimezoneAliases[text]
		if !ok {
			return nil, fmt.Errorf("无法识别的时区: %q", text)
		}
		tz = mapped
		rest = tzTextRegex.ReplaceAllString(rest, "")
	}
	matches := timeRangeRegex.FindAllStringSubmatchIndex(rest, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	// 标签 = 首个时间范围之前的文字；范围之后只允许分隔符与空白
	label := strings.TrimSpace(rest[:matches[0][0]])
	label = strings.TrimRight(label, "、，, ")
	if label == "" {
		return nil, fmt.Errorf("时段条件缺少标签: %q", cond)
	}
	tail := strings.TrimSpace(rest[matches[len(matches)-1][1]:])
	if strings.Trim(tail, "、，, ") != "" {
		return nil, fmt.Errorf("时段条件包含无法识别的内容: %q", cond)
	}
	ranges := make([]minuteRange, 0, len(matches))
	for _, idx := range matches {
		m := timeRangeRegex.FindStringSubmatch(rest[idx[0]:idx[1]])
		startH, _ := strconv.Atoi(m[1])
		startM, _ := strconv.Atoi(m[2])
		nextDay := m[3] != ""
		endH, _ := strconv.Atoi(m[4])
		endM, _ := strconv.Atoi(m[5])
		if startH > 23 || endH > 23 || startM > 59 || endM > 59 {
			return nil, fmt.Errorf("时段超出合法时间: %q", rest[idx[0]:idx[1]])
		}
		start := startH*60 + startM
		end := endH*60 + endM
		if start == end {
			return nil, fmt.Errorf("时段起止时间相同: %q", rest[idx[0]:idx[1]])
		}
		if nextDay && end > start {
			return nil, fmt.Errorf("次日标记但结束时间晚于开始时间: %q", rest[idx[0]:idx[1]])
		}
		ranges = append(ranges, minuteRange{Start: start, End: end})
	}
	return &timePeriodCond{Label: label, Timezone: tz, Ranges: ranges}, nil
}

// sameTimePeriod 判断两个时段条件的时区与范围是否一致（范围顺序无关）
func sameTimePeriod(a, b *timePeriodCond) bool {
	if a.Timezone != b.Timezone || len(a.Ranges) != len(b.Ranges) {
		return false
	}
	for _, ra := range a.Ranges {
		found := false
		for _, rb := range b.Ranges {
			if ra == rb {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// exprCondition 生成时段条件的表达式文本，按"分钟数"比较以兼容非整点。
// 单段：mins >= 540 && mins < 720；跨午夜：mins >= 1080 || mins < 480；多段以 || 连接。
func (tp *timePeriodCond) exprCondition() string {
	mins := fmt.Sprintf(`hour(%q) * 60 + minute(%q)`, tp.Timezone, tp.Timezone)
	parts := make([]string, 0, len(tp.Ranges))
	for _, r := range tp.Ranges {
		if r.End > r.Start {
			parts = append(parts, fmt.Sprintf("%s >= %d && %s < %d", mins, r.Start, mins, r.End))
		} else {
			parts = append(parts, fmt.Sprintf("%s >= %d || %s < %d", mins, r.Start, mins, r.End))
		}
	}
	return strings.Join(parts, " || ")
}

// formatPrice 输出最简十进制价格字符串，避免浮点尾巴（如 0.30000000000000004）
func formatPrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}

// priceColumns 必需字段在表头中的列索引
type priceColumns struct {
	modelID int
	desc    int
	price   int
	unit    int
}

// maxIndex 必需列中的最大列索引，用于判断数据行列数是否足够
func (c priceColumns) maxIndex() int {
	m := c.modelID
	for _, i := range []int{c.desc, c.price, c.unit} {
		if i > m {
			m = i
		}
	}
	return m
}

// locatePriceColumns 按表头名称定位必需列，兼容 5 列 CSV 格式
// （模型ID/计费单元/说明/标价(元)/单位）与含附加列（模型介绍、厂商等）的 Excel 格式
func locatePriceColumns(header []string) (priceColumns, error) {
	cols := priceColumns{modelID: -1, desc: -1, price: -1, unit: -1}
	for i, h := range header {
		switch h = strings.TrimSpace(h); {
		case h == "模型ID":
			cols.modelID = i
		case h == "说明":
			cols.desc = i
		case strings.HasPrefix(h, "标价"):
			cols.price = i
		case h == "单位":
			cols.unit = i
		}
	}
	if cols.modelID < 0 || cols.desc < 0 || cols.price < 0 || cols.unit < 0 {
		return cols, fmt.Errorf("表头缺少必需列（模型ID/说明/标价/单位），实际表头: %q", header)
	}
	return cols, nil
}

var errRecordTooShort = errors.New("记录列数不足")

// extractPriceRow 按列索引从一条数据记录提取价格行；
// 列数不足返回 errRecordTooShort，价格非法返回带原文的错误以便调用方记录日志
func extractPriceRow(record []string, cols priceColumns) (csvPriceRow, error) {
	if len(record) <= cols.maxIndex() {
		return csvPriceRow{}, errRecordTooShort
	}
	price, err := strconv.ParseFloat(strings.TrimSpace(record[cols.price]), 64)
	if err != nil || !isValidNonNegativeCost(price) {
		return csvPriceRow{}, fmt.Errorf("价格无效: %q", record[cols.price])
	}
	return csvPriceRow{
		ModelID: strings.TrimSpace(record[cols.modelID]),
		Desc:    strings.TrimSpace(record[cols.desc]),
		Price:   price,
		Unit:    strings.TrimSpace(record[cols.unit]),
	}, nil
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
	cols, err := locatePriceColumns(header)
	if err != nil {
		return nil, err
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
		row, err := extractPriceRow(record, cols)
		if errors.Is(err, errRecordTooShort) {
			continue
		}
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行%v", lineNum, err))
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseXLSXRows 解析 Excel(.xlsx) 第一个工作表的全部数据行，首行为表头。
// 必需列（模型ID/说明/标价/单位）按表头名称定位，允许存在其他附加列。
func parseXLSXRows(reader io.Reader, ctx context.Context) ([]csvPriceRow, error) {
	f, err := excelize.OpenReader(reader)
	if err != nil {
		return nil, fmt.Errorf("读取 Excel 文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("Excel 文件中没有工作表")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, fmt.Errorf("读取 Excel 工作表失败: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("Excel 文件为空")
	}
	cols, err := locatePriceColumns(records[0])
	if err != nil {
		return nil, err
	}

	var rows []csvPriceRow
	for i, record := range records[1:] {
		row, err := extractPriceRow(record, cols)
		if errors.Is(err, errRecordTooShort) {
			continue
		}
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("Excel 第 %d 行%v", i+2, err))
			continue
		}
		rows = append(rows, row)
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
	return buildTieredExpressionNamed(tiers, "")
}

// buildTieredExpressionNamed 与 buildTieredExpression 相同，但 namePrefix 非空时：
// 无阶梯求和会包一层 tier(namePrefix, ...) 以保留时段标签；有阶梯时档位名加此前缀，
// 用于时段分组表达式中区分各档位所属的时段。
func buildTieredExpressionNamed(tiers map[string][]priceTier, namePrefix string) string {
	bounds := collectTierBounds(tiers)
	if len(bounds) == 0 {
		sum := buildExprSum(tiers, 0)
		if namePrefix != "" {
			return fmt.Sprintf("tier(%q, %s)", namePrefix, sum)
		}
		return sum
	}
	names := tierNames(len(bounds))
	if namePrefix != "" {
		for i := range names {
			names[i] = namePrefix + " " + names[i]
		}
	}
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

// periodGroup 同一时段条件下按变量归类的档位价格；period 为 nil 表示无时段条件的默认组
type periodGroup struct {
	period *timePeriodCond
	tiers  map[string][]priceTier
}

// buildTimeSlicedExpression 由时段分组生成时间条件嵌套三元表达式。
// 兜底（else）分支：存在默认组（无时段条件）时为默认组，否则为最后出现的时段组（如谷时）。
func buildTimeSlicedExpression(groups []*periodGroup) string {
	fallbackIdx := len(groups) - 1
	for i, g := range groups {
		if g.period == nil {
			fallbackIdx = i
			break
		}
	}
	body := func(g *periodGroup) string {
		if g.period == nil {
			return buildTieredExpression(g.tiers)
		}
		return buildTieredExpressionNamed(g.tiers, g.period.Label)
	}
	expr := body(groups[fallbackIdx])
	for i := len(groups) - 1; i >= 0; i-- {
		if i == fallbackIdx {
			continue
		}
		thenBody := body(groups[i])
		if strings.Contains(thenBody, " ? ") {
			thenBody = "(" + thenBody + ")"
		}
		expr = fmt.Sprintf("%s ? %s : (%s)", groups[i].period.exprCondition(), thenBody, expr)
	}
	return expr
}

// buildPeriodDisplayLines 为多时段分组生成可读价格行，时段组的价格行标签带时段后缀，
// 如 {label: "Input price (峰时)", value: "3 元/M"}
func buildPeriodDisplayLines(groups []*periodGroup) []displayPriceLine {
	var lines []displayPriceLine
	for _, g := range groups {
		for _, l := range buildExprDisplayLines(g.tiers) {
			if g.period != nil {
				l.Label = fmt.Sprintf("%s (%s)", l.Label, g.period.Label)
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// buildTokenPricing 处理 token 类模型：简单模型出倍率，阶梯/多模态/含 cc1h 模型出表达式，
// 含峰谷时段条件的模型出时间条件嵌套三元表达式
func buildTokenPricing(modelID string, rows []csvPriceRow, result *csvConvertResult, ctx context.Context) {
	var groups []*periodGroup
	groupByLabel := make(map[string]*periodGroup)
	var defaultGroup *periodGroup

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
		// 时段与长度阶梯可能同时出现在条件中，先剥离阶梯部分再解析时段
		period, err := parseTimePeriodCondition(tierBoundRegex.ReplaceAllString(cond, ""))
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 时段条件无效，已跳过 %q: %v", modelID, r.Desc, err))
			continue
		}
		var g *periodGroup
		if period == nil {
			if defaultGroup == nil {
				defaultGroup = &periodGroup{tiers: make(map[string][]priceTier)}
				groups = append(groups, defaultGroup)
			}
			g = defaultGroup
		} else {
			g = groupByLabel[period.Label]
			if g != nil {
				if !sameTimePeriod(g.period, period) {
					logger.LogWarn(ctx, fmt.Sprintf("模型 %s 时段标签 %q 的时间范围不一致，已跳过 %q", modelID, period.Label, r.Desc))
					continue
				}
			} else {
				g = &periodGroup{period: period, tiers: make(map[string][]priceTier)}
				groupByLabel[period.Label] = g
				groups = append(groups, g)
			}
		}
		g.tiers[varName] = append(g.tiers[varName], priceTier{UpperBound: bound, Price: r.Price})
	}

	if len(groups) == 0 {
		return
	}

	// 统一走直接单价表达式：p*输入价 + c*输出价 + cr*缓存价 + cc*写缓存价
	// 表达式系数即 CSV 人民币价格，不再走 model_ratio/completion_ratio 倍率换算
	var expr string
	var displayLines []displayPriceLine
	if len(groups) == 1 && groups[0].period == nil {
		expr = buildTieredExpression(groups[0].tiers)
		displayLines = buildExprDisplayLines(groups[0].tiers)
	} else {
		expr = buildTimeSlicedExpression(groups)
		displayLines = buildPeriodDisplayLines(groups)
	}
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
	result.setDisplayPrice(modelID, "billing_expr", displayLines)
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

// convertCSVToRatioData 将 CSV 内容转换为同步数据格式，仅保留 localModels 中存在的模型
func convertCSVToRatioData(reader io.Reader, localModels map[string]bool, ctx context.Context) (map[string]any, map[string]map[string]any, []string, error) {
	rows, err := parseCSVRows(reader, ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return convertRowsToRatioData(rows, localModels, ctx)
}

// convertRowsToRatioData 将解析出的价格行转换为同步数据格式，仅保留 localModels 中存在的模型。
// 返回的 map key 与 pricingSyncFields 对齐，可直接交给 buildDifferences 复用；
// 同时返回每个模型各字段的可读价格展示内容，供差异对比界面展示。
func convertRowsToRatioData(rows []csvPriceRow, localModels map[string]bool, ctx context.Context) (map[string]any, map[string]map[string]any, []string, error) {
	if len(rows) == 0 {
		return nil, nil, nil, fmt.Errorf("文件中无有效数据行")
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

// FetchCSVUpstreamRatios 接收上传的 CSV/Excel 文件，解析后复用差异对比流程返回结果
func FetchCSVUpstreamRatios(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "请选择要上传的 CSV 或 Excel 文件"})
		return
	}
	if fileHeader.Size > maxCSVFileSize {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件大小不能超过 10MB"})
		return
	}
	filename := strings.ToLower(fileHeader.Filename)
	isCSV := strings.HasSuffix(filename, ".csv")
	isXLSX := strings.HasSuffix(filename, ".xlsx")
	if !isCSV && !isXLSX {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "仅支持 .csv 或 .xlsx 文件"})
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

	var rows []csvPriceRow
	if isXLSX {
		rows, err = parseXLSXRows(file, c.Request.Context())
	} else {
		rows, err = parseCSVRows(file, c.Request.Context())
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件解析失败: " + err.Error()})
		return
	}

	converted, displayPrices, skippedModels, err := convertRowsToRatioData(rows, localModels, c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件解析失败: " + err.Error()})
		return
	}

	successfulChannels := []struct {
		name string
		data map[string]any
	}{{name: csvImportChannelName, data: converted}}
	differences := buildDifferences(localData, successfulChannels)

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("定价文件导入解析完成: 跳过 %d 个渠道中不存在的模型", len(skippedModels)))

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
