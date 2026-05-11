package service

import (
	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/shopspring/decimal"
)

// MinimumChargeResult 描述保底消费的应用结果，供日志展示使用。
type MinimumChargeResult struct {
	Applied       bool    // 是否真正应用了保底（floor 提升了金额）
	MinCharge     float64 // 保底价格（$）
	MinQuota      int     // 换算后的保底 quota
	OriginalQuota int     // 应用前的原始 quota
}

// ApplyMinimumCharge 根据模型配置对最终 quota 应用保底消费。
// 约束：
//   - totalTokens == 0（上游异常/超时）不应用，直接返回 currentQuota。
//   - relayInfo.PriceData.FreeModel 或 GroupRatio == 0（免费模型）不应用。
//   - 保底以美元配置，换算 quota = minCharge * QuotaPerUnit（不乘分组倍率）。
//   - 仅在 currentQuota < minQuota 时拉高到 minQuota。
func ApplyMinimumCharge(relayInfo *relaycommon.RelayInfo, currentQuota int, totalTokens int) (int, MinimumChargeResult) {
	result := MinimumChargeResult{OriginalQuota: currentQuota}

	if relayInfo == nil || totalTokens <= 0 {
		return currentQuota, result
	}
	if relayInfo.PriceData.FreeModel {
		return currentQuota, result
	}
	if relayInfo.PriceData.GroupRatioInfo.GroupRatio == 0 {
		return currentQuota, result
	}

	minCharge, ok := billing_setting.GetMinimumCharge(relayInfo.OriginModelName)
	if !ok {
		return currentQuota, result
	}

	minQuotaDecimal := decimal.NewFromFloat(minCharge).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	minQuota := int(minQuotaDecimal.Round(0).IntPart())

	result.MinCharge = minCharge
	result.MinQuota = minQuota

	if currentQuota >= minQuota {
		return currentQuota, result
	}

	result.Applied = true
	return minQuota, result
}
