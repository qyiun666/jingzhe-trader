package config

import "strings"

// maskLiteral 掩码替换文本（所有输出通道默认以此替换凭据明文）。
const maskLiteral = "****"

// Mask 将任意凭据值替换为掩码。空值返回空（避免泄露"存在但为空"的信号）。
func Mask(value string) string {
	if value == "" {
		return ""
	}
	return maskLiteral
}

// envName 由配置键推导环境变量名：JZ_ + 大写 + 点号转下划线。
// 例：tushare.token → JZ_TUSHARE_TOKEN；server.api_token → JZ_SERVER_API_TOKEN。
func envName(key string) string {
	return "JZ_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}
