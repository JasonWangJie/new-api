package controller

import (
	"net/mail"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

type createInvoiceRequest struct {
	OrderIDs    []int  `json:"order_ids"`
	CompanyName string `json:"company_name"`
	TaxID       string `json:"tax_id"`
	Email       string `json:"email"`
}

type rejectInvoiceRequest struct {
	Reason string `json:"reason"`
}

type historicalInvoiceRequest struct {
	UserID   int    `json:"user_id"`
	OrderIDs []int  `json:"order_ids"`
	Note     string `json:"note"`
}

func invoiceStatusValid(status string) bool {
	return status == "" || status == model.InvoiceStatusPending || status == model.InvoiceStatusCompleted || status == model.InvoiceStatusRejected
}

func invoiceRequestID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "无效的开票申请编号")
		return 0, false
	}
	return id, true
}

func GetInvoiceConfig(c *gin.Context) {
	setting := operation_setting.GetPaymentSetting()
	profile, err := model.GetLatestUserInvoiceProfile(c.GetInt("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"enabled":              setting.InvoiceEnabled,
		"min_amount_cents":     setting.InvoiceMinAmountCents,
		"last_invoice_profile": profile,
	})
}

func GetInvoiceEligibleOrders(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	orders, total, err := model.ListInvoiceEligibleOrders(c.GetInt("id"), c.Query("keyword"), pageInfo)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(orders)
	common.ApiSuccess(c, pageInfo)
}

func CreateInvoiceRequest(c *gin.Context) {
	setting := operation_setting.GetPaymentSetting()
	if !setting.InvoiceEnabled {
		common.ApiError(c, model.ErrInvoiceDisabled)
		return
	}
	var request createInvoiceRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorMsg(c, "无效的申请参数")
		return
	}
	request.CompanyName = strings.TrimSpace(request.CompanyName)
	request.TaxID = strings.TrimSpace(request.TaxID)
	request.Email = strings.TrimSpace(request.Email)
	address, emailErr := mail.ParseAddress(request.Email)
	if request.CompanyName == "" || utf8.RuneCountInString(request.CompanyName) > 200 {
		common.ApiErrorMsg(c, "企业名称不能为空且不能超过 200 个字符")
		return
	}
	if request.TaxID == "" || utf8.RuneCountInString(request.TaxID) > 64 {
		common.ApiErrorMsg(c, "企业税号不能为空且不能超过 64 个字符")
		return
	}
	if emailErr != nil || address.Address != request.Email || len(request.Email) > 254 {
		common.ApiErrorMsg(c, "请输入有效的邮箱地址")
		return
	}
	created, err := model.CreateUserInvoiceRequest(
		c.GetInt("id"), request.OrderIDs, request.CompanyName, request.TaxID,
		request.Email, setting.InvoiceMinAmountCents,
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, created)
}

func ListMyInvoiceRequests(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	status := c.Query("status")
	if !invoiceStatusValid(status) {
		common.ApiErrorMsg(c, "无效的开票状态")
		return
	}
	requests, total, err := model.ListInvoiceRequests(c.GetInt("id"), status, c.Query("keyword"), pageInfo)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(requests)
	common.ApiSuccess(c, pageInfo)
}

func GetMyInvoiceRequest(c *gin.Context) {
	id, ok := invoiceRequestID(c)
	if !ok {
		return
	}
	request, err := model.GetInvoiceRequest(id, c.GetInt("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, request)
}

func AdminListInvoiceRequests(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	status := c.Query("status")
	if !invoiceStatusValid(status) {
		common.ApiErrorMsg(c, "无效的开票状态")
		return
	}
	requests, total, err := model.ListInvoiceRequests(0, status, c.Query("keyword"), pageInfo)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(requests)
	common.ApiSuccess(c, pageInfo)
}

func AdminGetInvoiceRequest(c *gin.Context) {
	id, ok := invoiceRequestID(c)
	if !ok {
		return
	}
	request, err := model.GetInvoiceRequest(id, 0)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, request)
}

func AdminGetInvoiceEligibleOrders(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		common.ApiErrorMsg(c, "请选择有效用户")
		return
	}
	pageInfo := common.GetPageQuery(c)
	orders, total, err := model.ListInvoiceEligibleOrders(userID, c.Query("keyword"), pageInfo)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(orders)
	common.ApiSuccess(c, pageInfo)
}

func AdminCompleteInvoiceRequest(c *gin.Context) {
	id, ok := invoiceRequestID(c)
	if !ok {
		return
	}
	processed, err := model.ProcessInvoiceRequest(id, model.InvoiceStatusCompleted, "", model.InvoiceOperator{
		Id: c.GetInt("id"), Username: c.GetString("username"),
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAuditFor(c, processed.UserId, "invoice.complete", map[string]any{"request_id": processed.Id})
	common.ApiSuccess(c, processed)
}

func AdminRejectInvoiceRequest(c *gin.Context) {
	id, ok := invoiceRequestID(c)
	if !ok {
		return
	}
	var request rejectInvoiceRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorMsg(c, "无效的拒绝参数")
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" || utf8.RuneCountInString(request.Reason) > 1000 {
		common.ApiErrorMsg(c, "拒绝原因不能为空且不能超过 1000 个字符")
		return
	}
	processed, err := model.ProcessInvoiceRequest(id, model.InvoiceStatusRejected, request.Reason, model.InvoiceOperator{
		Id: c.GetInt("id"), Username: c.GetString("username"),
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAuditFor(c, processed.UserId, "invoice.reject", map[string]any{"request_id": processed.Id})
	common.ApiSuccess(c, processed)
}

func AdminCreateHistoricalInvoice(c *gin.Context) {
	var request historicalInvoiceRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || request.UserID <= 0 {
		common.ApiErrorMsg(c, "无效的历史补开参数")
		return
	}
	request.Note = strings.TrimSpace(request.Note)
	if utf8.RuneCountInString(request.Note) > 1000 {
		common.ApiErrorMsg(c, "备注不能超过 1000 个字符")
		return
	}
	created, err := model.CreateHistoricalInvoiceRequest(request.UserID, request.OrderIDs, request.Note, model.InvoiceOperator{
		Id: c.GetInt("id"), Username: c.GetString("username"),
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAuditFor(c, request.UserID, "invoice.history", map[string]any{"request_id": created.Id})
	common.ApiSuccess(c, created)
}
