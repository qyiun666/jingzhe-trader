package analysis

import (
	"regexp"
	"strings"
	"unicode"

	"jingzhe-trader/internal/model"
)

// NewsAnalyzer 新闻分析器
// 提供简单的关键词提取、股票匹配和情感打分功能
type NewsAnalyzer struct{}

// NewNewsAnalyzer 构造 NewsAnalyzer
func NewNewsAnalyzer() *NewsAnalyzer {
	return &NewsAnalyzer{}
}

// 常见中文停用词
var stopWords = map[string]bool{
	"的": true, "了": true, "在": true, "是": true, "我": true, "有": true,
	"和": true, "就": true, "不": true, "人": true, "都": true, "一": true,
	"一个": true, "上": true, "也": true, "很": true, "到": true, "说": true,
	"要": true, "去": true, "你": true, "会": true, "着": true, "没有": true,
	"看": true, "好": true, "自己": true, "这": true, "那": true, "之": true,
	"与": true, "及": true, "等": true, "对": true, "可以": true, "为": true,
	"将": true, "于": true, "但": true, "而": true, "被": true, "其": true,
	"并": true, "从": true, "该": true, "以": true, "或": true, "公司": true,
	"股份": true, "集团": true, "有限": true, "有限公司": true,
}

// 正面情感词
var positiveWords = map[string]float64{
	"涨": 0.5, "上涨": 0.6, "大涨": 0.8, "涨停": 0.9,
	"利好": 0.8, "重大利好": 1.0, "突破": 0.6, "创新高": 0.8,
	"增长": 0.5, "盈利": 0.5, "超预期": 0.7, "看好": 0.6,
	"买入": 0.5, "增持": 0.5, "推荐": 0.5, "龙头": 0.4,
	"强劲": 0.6, "复苏": 0.5, "繁荣": 0.6, "扩张": 0.4,
}

// 负面情感词
var negativeWords = map[string]float64{
	"跌": -0.5, "下跌": -0.6, "大跌": -0.8, "跌停": -0.9,
	"利空": -0.8, "重大利空": -1.0, "暴雷": -0.9, "亏损": -0.7,
	"下降": -0.5, "不及预期": -0.7, "看空": -0.6, "卖出": -0.5,
	"减持": -0.5, "抛售": -0.7, "退市": -0.9, "崩盘": -1.0,
	"风险": -0.4, "下滑": -0.5, "萎缩": -0.5, "裁员": -0.4,
}

// punctuationReplacer 用于将标点替换为空格
var punctuationReplacer = strings.NewReplacer(
	"，", " ", "。", " ", "！", " ", "？", " ",
	"；", " ", "：", " ", "\"", " ", "\"", " ",
	"【", " ", "】", " ", "（", " ", "）", " ",
	"《", " ", "》", " ", "、", " ", ",", " ",
	".", " ", "!", " ", "?", " ", ";", " ",
	":", " ", "(", " ", ")", " ", "[", " ", "]", " ",
	"/", " ", "-", " ", "_", " ",
)

// splitChars 将中英文文本拆分为词单元
// 英文按空格和标点拆分, 中文按字拆分
func splitChars(text string) []string {
	var parts []string
	var current strings.Builder

	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			parts = append(parts, string(r))
		} else if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// ExtractKeywords 从新闻内容提取关键词(简单分词, 过滤停用词和单字)
func (na *NewsAnalyzer) ExtractKeywords(content string) []string {
	if content == "" {
		return nil
	}
	// 统一替换标点为空格
	text := punctuationReplacer.Replace(content)
	parts := splitChars(text)

	keywordSet := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 过滤单字(除汉字外)
		if len([]rune(p)) == 1 {
			r := []rune(p)[0]
			if !unicode.Is(unicode.Han, r) {
				continue
			}
		}
		// 过滤停用词
		if stopWords[p] {
			continue
		}
		// 过滤纯数字
		if isAllDigits(p) {
			continue
		}
		keywordSet[p] = true
	}

	keywords := make([]string, 0, len(keywordSet))
	for k := range keywordSet {
		keywords = append(keywords, k)
	}
	return keywords
}

// MatchStocks 将新闻与股票代码匹配(基于股票名称和关键词的简单匹配)
func (na *NewsAnalyzer) MatchStocks(newsList []model.News, stocks map[string]*model.Stock) map[string][]model.News {
	result := make(map[string][]model.News)
	for _, n := range newsList {
		text := n.Title + " " + n.Content
		keywords := na.ExtractKeywords(text)
		keywordSet := make(map[string]bool)
		for _, kw := range keywords {
			keywordSet[kw] = true
		}

		matched := make(map[string]bool)
		for tsCode, stock := range stocks {
			if matched[tsCode] {
				continue
			}
			// 匹配股票名称
			if stock.Name != "" && strings.Contains(text, stock.Name) {
				result[tsCode] = append(result[tsCode], n)
				matched[tsCode] = true
				continue
			}
			// 匹配股票代码(去掉后缀)
			symbol := strings.Split(tsCode, ".")[0]
			if keywordSet[symbol] {
				result[tsCode] = append(result[tsCode], n)
				matched[tsCode] = true
			}
		}
	}
	return result
}

// SentimentScore 简单情感打分(-1到1)
// 基于内置的正负词表, 对内容中出现的情感词加权求和并归一化
func (na *NewsAnalyzer) SentimentScore(content string) float64 {
	if content == "" {
		return 0
	}
	score := 0.0
	// 先匹配多字词, 再匹配单字
	for word, weight := range positiveWords {
		if strings.Contains(content, word) {
			score += weight
		}
	}
	for word, weight := range negativeWords {
		if strings.Contains(content, word) {
			score += weight
		}
	}
	// 简单截断到 [-1, 1]
	if score > 1 {
		score = 1
	}
	if score < -1 {
		score = -1
	}
	return score
}

// isAllDigits 判断字符串是否全为数字
func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

// regexpNonWord 用于替换非单词字符
var regexpNonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)
