package core

import "fmt"

// Error is a domain error with a stable API code, a user-facing message and an
// HTTP status. The HTTP layer renders it as {"error":{"code":..,"message":..}}.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds a domain error. Status defaults to 400 when omitted.
func NewError(code, message string, status int) *Error {
	if status == 0 {
		status = 400
	}
	return &Error{Code: code, Message: message, Status: status}
}

// Common domain errors shared by the API, worker and dispatcher.
var (
	ErrAuthRequired    = NewError("AUTH_REQUIRED", "请先登录", 401)
	ErrForbidden       = NewError("FORBIDDEN", "需要管理员权限", 403)
	ErrNotFound        = NewError("NOT_FOUND", "资源不存在", 404)
	ErrJobLimit        = NewError("JOB_LIMIT_REACHED", "未完成任务数量已达上限", 409)
	ErrDiskLow         = NewError("DISK_LOW", "存储空间不足，暂时无法接受新任务", 503)
	ErrUploadTooLarge  = NewError("UPLOAD_TOO_LARGE", "请求体超过大小限制", 413)
	ErrInvalidRange    = NewError("INVALID_RANGE", "无效字节范围", 416)
	ErrModelNotReady   = NewError("MODEL_NOT_READY", "本地模型未准备就绪", 503)
	ErrEngineNotReady  = NewError("ENGINE_NOT_AVAILABLE", "该引擎尚未验证可用", 400)
	ErrArtifactMissing = NewError("ARTIFACT_NOT_AVAILABLE", "文件暂不可用", 404)
)
