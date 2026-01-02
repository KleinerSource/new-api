package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/custom/relay"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// ChatStreamRequest 加密请求格式
type ChatStreamRequest struct {
	EncryptedData string   `json:"encrypted_data"`
	IV            string   `json:"iv"`
	Data          string   `json:"data"`
	Images        []string `json:"images"`
	Model         string   `json:"model"`
}

// RelayPassthrough 传透模式接口处理器
func RelayPassthrough(c *gin.Context) {
	requestId := c.GetString(common.RequestIdKey)

	var newAPIError *types.NewAPIError

	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("passthrough relay error: %s", newAPIError.Error()))
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			c.JSON(newAPIError.StatusCode, gin.H{
				"error": newAPIError.ToOpenAIError(),
			})
		}
	}()

	var chatStreamReq ChatStreamRequest
	if err := common.UnmarshalBodyReusable(c, &chatStreamReq); err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		return
	}

	if chatStreamReq.Model == "" {
		newAPIError = types.NewError(errors.New("model is required"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		return
	}

	if setting.ShouldCheckPromptSensitive() && chatStreamReq.Data != "" {
		contains, words := service.CheckSensitiveText(chatStreamReq.Data)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("passthrough sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(errors.New("sensitive words detected"), types.ErrorCodeSensitiveWordsDetected, types.ErrOptionWithSkipRetry())
			return
		}
	}

	relayInfo := genPassthroughRelayInfo(c, chatStreamReq.Model, true)
	estimatedInputTokens := calculateInputTokens(chatStreamReq, relayInfo.OriginModelName)
	relayInfo.SetEstimatePromptTokens(estimatedInputTokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, estimatedInputTokens, &types.TokenCountMeta{})
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError)
		return
	}

	if !priceData.FreeModel {
		newAPIError = service.PreConsumeQuota(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		if newAPIError != nil && relayInfo.FinalPreConsumedQuota != 0 {
			service.ReturnPreConsumedQuota(c, relayInfo)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		channel, channelErr := getPassthroughChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			newAPIError = channelErr
			break
		}

		addUsedChannel(c, channel.Id)
		relayInfo.InitChannelMeta(c)

		if err := applyModelMapping(c, relayInfo); err != nil {
			newAPIError = types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
			break
		}

		requestBody, bodyErr := common.GetRequestBody(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			break
		}
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))

		var passthroughResult *relay.PassthroughResult
		passthroughResult, newAPIError = relay.PassthroughHelperWithUsage(c, relayInfo)

		if newAPIError == nil {
			postPassthroughConsumeQuotaWithResult(c, relayInfo, passthroughResult, estimatedInputTokens)
			return
		}

		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)

		if !shouldRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
}

// genPassthroughRelayInfo 生成传透模式的 RelayInfo
func genPassthroughRelayInfo(c *gin.Context, modelName string, isStream bool) *relaycommon.RelayInfo {
	// 设置必要的上下文信息，供 GenRelayInfoOpenAI 使用
	c.Set(string(constant.ContextKeyOriginalModel), modelName)

	// 创建一个简单的 request 实现来传递 stream 信息
	mockRequest := &passthroughRequest{stream: isStream}

	// 使用标准方法生成 RelayInfo，确保 isFirstResponse 等私有字段正确初始化
	info := relaycommon.GenRelayInfoOpenAI(c, mockRequest)
	info.OriginModelName = modelName
	info.IsStream = isStream
	info.RequestURLPath = "/chat-stream"

	return info
}

// passthroughRequest 实现 dto.Request 接口的最小实现
type passthroughRequest struct {
	stream bool
}

func (r *passthroughRequest) IsStream(c *gin.Context) bool {
	return r.stream
}

func (r *passthroughRequest) GetTokenCountMeta() *types.TokenCountMeta {
	return &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
}

func (r *passthroughRequest) SetModelName(modelName string) {
	// 不需要实现
}

// isBugmentChannel 检查渠道标签是否包含 "bugment"（不区分大小写）
func isBugmentChannel(channel *model.Channel) bool {
	if channel == nil {
		return false
	}
	tag := channel.GetTag()
	return strings.Contains(strings.ToLower(tag), "bugment")
}

// ErrNoBugmentChannel 没有可用的 bugment 渠道错误
var ErrNoBugmentChannel = errors.New("此接口仅支持标签包含 bugment 的渠道")

// getRandomBugmentChannel 获取指定 group 和 model 下标签包含 bugment 的渠道
// 支持重试机制，retry 参数用于在多个渠道间切换优先级
func getRandomBugmentChannel(group string, modelName string, retry int) (*model.Channel, error) {
	// 构建兼容不同数据库的查询条件
	groupCol := "`group`"
	tagCol := "`tag`"
	modelsCol := "`models`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		tagCol = `"tag"`
		modelsCol = `"models"`
	}

	var groupCondition string
	if common.UsingMySQL {
		groupCondition = fmt.Sprintf("CONCAT(',', %s, ',') LIKE ?", groupCol)
	} else {
		groupCondition = fmt.Sprintf("(',' || %s || ',') LIKE ?", groupCol)
	}

	// 构建模型查询条件
	var modelCondition string
	if common.UsingMySQL {
		modelCondition = fmt.Sprintf("CONCAT(',', %s, ',') LIKE ?", modelsCol)
	} else {
		modelCondition = fmt.Sprintf("(',' || %s || ',') LIKE ?", modelsCol)
	}

	// 构建标签查询条件（不区分大小写）
	tagCondition := fmt.Sprintf("LOWER(%s) LIKE ?", tagCol)

	// 查询该 Group 和 Model 下所有标签包含 bugment 的渠道
	var channels []*model.Channel
	err := model.DB.Where("status = ?", 1).
		Where(groupCondition, "%,"+group+",%").
		Where(modelCondition, "%,"+modelName+",%").
		Where(tagCondition, "%bugment%").
		Order("priority DESC").
		Find(&channels).Error

	if err != nil {
		return nil, fmt.Errorf("查询渠道失败: %w", err)
	}

	// 进一步过滤，确保标签包含 bugment（双重验证）
	var bugmentChannels []*model.Channel
	for _, ch := range channels {
		if isBugmentChannel(ch) {
			bugmentChannels = append(bugmentChannels, ch)
		}
	}

	if len(bugmentChannels) == 0 {
		return nil, ErrNoBugmentChannel
	}

	// 根据优先级和权重选择渠道
	return selectChannelByPriorityAndWeight(bugmentChannels, retry)
}

// selectChannelByPriorityAndWeight 根据优先级和权重选择渠道
func selectChannelByPriorityAndWeight(channels []*model.Channel, retry int) (*model.Channel, error) {
	if len(channels) == 0 {
		return nil, ErrNoBugmentChannel
	}

	// 获取所有不同的优先级
	prioritySet := make(map[int64]bool)
	for _, ch := range channels {
		prioritySet[ch.GetPriority()] = true
	}

	// 排序优先级（降序）
	var priorities []int64
	for p := range prioritySet {
		priorities = append(priorities, p)
	}
	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i] > priorities[j]
	})

	// 根据 retry 选择优先级
	priorityIndex := retry
	if priorityIndex >= len(priorities) {
		priorityIndex = len(priorities) - 1
	}
	targetPriority := priorities[priorityIndex]

	// 获取该优先级下的所有渠道
	var targetChannels []*model.Channel
	var totalWeight int
	for _, ch := range channels {
		if ch.GetPriority() == targetPriority {
			targetChannels = append(targetChannels, ch)
			totalWeight += ch.GetWeight()
		}
	}

	if len(targetChannels) == 0 {
		return nil, ErrNoBugmentChannel
	}

	if len(targetChannels) == 1 {
		return targetChannels[0], nil
	}

	// 按权重随机选择
	if totalWeight == 0 {
		return targetChannels[rand.Intn(len(targetChannels))], nil
	}

	randomWeight := rand.Intn(totalWeight)
	for _, ch := range targetChannels {
		randomWeight -= ch.GetWeight()
		if randomWeight < 0 {
			return ch, nil
		}
	}

	return targetChannels[0], nil
}

// getPassthroughChannel 获取传透模式的渠道（仅支持标签包含 bugment 的渠道）
func getPassthroughChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		// 直接指定渠道的情况，检查渠道标签是否包含 bugment
		channelTag := c.GetString("channel_tag")
		if !strings.Contains(strings.ToLower(channelTag), "bugment") {
			return nil, types.NewError(errors.New("此接口仅支持标签包含 bugment 的渠道"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		}
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			Tag:     &channelTag,
			AutoBan: &autoBanInt,
		}, nil
	}

	// 使用专门的 Bugment 渠道选择函数（只从标签包含 bugment 的渠道中选择）
	channel, err := getRandomBugmentChannel(retryParam.TokenGroup, retryParam.ModelName, retryParam.GetRetry())
	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的标签包含 bugment 的渠道失败: %s", retryParam.TokenGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的标签包含 bugment 的渠道不存在", retryParam.TokenGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

// applyModelMapping 应用模型映射
func applyModelMapping(c *gin.Context, info *relaycommon.RelayInfo) error {
	return helper.ModelMappedHelper(c, info, nil)
}

// calculateInputTokens 精确计算输入 token 数量
func calculateInputTokens(req ChatStreamRequest, modelName string) int {
	totalTokens := 0

	if req.Data != "" {
		totalTokens += service.CountTextToken(req.Data, modelName)
	}

	for _, imageData := range req.Images {
		if imageData == "" {
			continue
		}
		fileMeta := &types.FileMeta{
			FileType:   types.FileTypeImage,
			OriginData: imageData,
		}
		imageTokens, err := service.GetImageTokenForPassthrough(fileMeta, modelName)
		if err != nil {
			common.SysLog(fmt.Sprintf("calculate image token failed: %v, using default 500", err))
			imageTokens = 500
		}
		totalTokens += imageTokens
	}

	if req.Model != "" {
		totalTokens += service.CountTextToken(req.Model, modelName)
	}

	if totalTokens < 1 {
		totalTokens = 1
	}

	return totalTokens
}

// addUsedChannel 记录使用的渠道
func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

// shouldRetry 判断是否应该重试
func shouldRetry(c *gin.Context, err *types.NewAPIError, retryTimesLeft int) bool {
	if err == nil || types.IsSkipRetryError(err) || retryTimesLeft <= 0 {
		return false
	}
	return true
}

// processChannelError 处理渠道错误
func processChannelError(c *gin.Context, channelErr types.ChannelError, apiErr *types.NewAPIError) {
	logger.LogError(c, fmt.Sprintf("channel %d (%s) error: %s", channelErr.ChannelId, channelErr.ChannelName, apiErr.Error()))
}

// postPassthroughConsumeQuotaWithResult 传透模式的消费记录
func postPassthroughConsumeQuotaWithResult(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, result *relay.PassthroughResult, estimatedInputTokens int) {
	useTimeSeconds := time.Now().Unix() - relayInfo.StartTime.Unix()
	tokenName := ctx.GetString("token_name")

	var promptTokens, completionTokens int
	var logContent string

	// 判断上游是否返回有效的 usage
	hasValidUsage := result != nil && result.Usage != nil &&
		(result.Usage.PromptTokens > 0 || result.Usage.CompletionTokens > 0)

	// 调试日志：输出 result 状态
	if common.DebugEnabled {
		if result != nil {
			logger.LogDebug(ctx, fmt.Sprintf("[Passthrough Billing] hasValidUsage=%v, ResponseContent length=%d",
				hasValidUsage, len(result.ResponseContent)))
			if result.Usage != nil {
				logger.LogDebug(ctx, fmt.Sprintf("[Passthrough Billing] Upstream usage: prompt=%d, completion=%d",
					result.Usage.PromptTokens, result.Usage.CompletionTokens))
			}
		} else {
			logger.LogDebug(ctx, "[Passthrough Billing] result is nil")
		}
	}

	if hasValidUsage {
		// 使用上游提供的 usage
		promptTokens = result.Usage.PromptTokens
		completionTokens = result.Usage.CompletionTokens
		logContent = "传透模式（上游计费）"
	} else {
		// 上游无 usage，使用本地估算
		promptTokens = estimatedInputTokens

		// 从响应内容计算输出 token
		if result != nil && result.ResponseContent != "" {
			completionTokens = service.CountTextToken(result.ResponseContent, relayInfo.OriginModelName)
			logContent = "传透模式（上游无 usage，本地估算）"

			// 调试日志：输出本地计算结果
			if common.DebugEnabled {
				logger.LogDebug(ctx, fmt.Sprintf("[Passthrough Billing] Local token count: prompt=%d, completion=%d, model=%s",
					promptTokens, completionTokens, relayInfo.OriginModelName))
			}
		} else {
			completionTokens = 0
			logContent = "传透模式（上游无 usage，无响应内容）"

			// 调试日志：无响应内容
			if common.DebugEnabled {
				logger.LogDebug(ctx, "[Passthrough Billing] No response content for local token calculation")
			}
		}
	}

	modelRatio := relayInfo.PriceData.ModelRatio
	groupRatio := relayInfo.PriceData.GroupRatioInfo.GroupRatio
	completionRatio := relayInfo.PriceData.CompletionRatio
	modelPrice := relayInfo.PriceData.ModelPrice
	usePrice := relayInfo.PriceData.UsePrice
	userGroupRatio := relayInfo.PriceData.GroupRatioInfo.GroupSpecialRatio

	var quota int

	if !usePrice {
		calculateQuota := float64(promptTokens) + float64(completionTokens)*completionRatio
		calculateQuota = calculateQuota * groupRatio * modelRatio
		quota = int(calculateQuota)
		logContent += fmt.Sprintf("，模型倍率 %.2f，补全倍率 %.2f，分组倍率 %.2f", modelRatio, completionRatio, groupRatio)
	} else {
		quota = int(modelPrice * common.QuotaPerUnit * groupRatio)
		logContent += fmt.Sprintf("，模型价格 %.2f，分组倍率 %.2f", modelPrice, groupRatio)
	}

	quotaDelta := quota - relayInfo.FinalPreConsumedQuota

	if quotaDelta > 0 {
		logger.LogInfo(ctx, fmt.Sprintf("传透模式预扣费后补扣费：%s（实际消耗：%s，预扣费：%s）",
			logger.FormatQuota(quotaDelta),
			logger.FormatQuota(quota),
			logger.FormatQuota(relayInfo.FinalPreConsumedQuota),
		))
	} else if quotaDelta < 0 {
		logger.LogInfo(ctx, fmt.Sprintf("传透模式预扣费后返还扣费：%s（实际消耗：%s，预扣费：%s）",
			logger.FormatQuota(-quotaDelta),
			logger.FormatQuota(quota),
			logger.FormatQuota(relayInfo.FinalPreConsumedQuota),
		))
	}

	if quotaDelta != 0 {
		err := service.PostConsumeQuota(relayInfo, quotaDelta, relayInfo.FinalPreConsumedQuota, true)
		if err != nil {
			logger.LogError(ctx, "error consuming token remain quota: "+err.Error())
		}
	}

	if quota > 0 {
		model.UpdateUserUsedQuotaAndRequestCount(relayInfo.UserId, quota)
		model.UpdateChannelUsedQuota(relayInfo.ChannelId, quota)
	}

	other := service.GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, 0, 0.0, modelPrice, userGroupRatio)
	other["passthrough"] = true
	if result != nil && result.Usage != nil {
		other["usage"] = result.Usage
	}

	model.RecordConsumeLog(ctx, relayInfo.UserId, model.RecordConsumeLogParams{
		ChannelId:        relayInfo.ChannelId,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		ModelName:        relayInfo.OriginModelName,
		TokenName:        tokenName,
		Quota:            quota,
		Content:          logContent,
		TokenId:          relayInfo.TokenId,
		UseTimeSeconds:   int(useTimeSeconds),
		IsStream:         relayInfo.IsStream,
		Group:            relayInfo.UsingGroup,
		Other:            other,
	})
}

