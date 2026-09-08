package inpaint

import (
	"encoding/json"
	"image"
)

// 协议错误码（与 python_engine/worker.py 约定一致）。
const (
	CodeModelLoadFailed   = "MODEL_LOAD_FAILED"
	CodeEngineStartFailed = "ENGINE_START_FAILED"
	CodeEngineTimeout     = "ENGINE_TIMEOUT"
	CodeEngineCrashed     = "ENGINE_CRASHED"
	CodeInferFailed       = "INFER_FAILED"
	CodeInvalidRequest    = "INVALID_REQUEST"
	CodeInvalidImage      = "INVALID_IMAGE"
	CodeCanceled          = "CANCELED"
)

// 协议消息类型。
const (
	MsgHello    = "hello"
	MsgInfer    = "infer"
	MsgPing     = "ping"
	MsgCancel   = "cancel"
	MsgShutdown = "shutdown"
	MsgEvent    = "event"
)

// ErrorInfo 协议错误信封字段。
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Message 统一 JSONL 信封：{type,id,ok,data,error}。
// Data 使用 map[string]any 以兼容 hello/infer/event 等不同载荷。
type Message struct {
	Type  string         `json:"type"`
	ID    int64          `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error *ErrorInfo     `json:"error,omitempty"`
}

// NewRequest 构造一个请求消息（Data 可为 nil）。
func NewRequest(typ string, id int64, data map[string]any) *Message {
	return &Message{Type: typ, ID: id, Data: data}
}

// EncodeMessage 序列化为一行 JSON（不含换行符）。
func EncodeMessage(m *Message) ([]byte, error) {
	return json.Marshal(m)
}

// DecodeMessage 从一行 JSON 解析信封。
func DecodeMessage(line []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ErrorCode 返回错误码，无错误时返回空串。
func (m *Message) ErrorCode() string {
	if m.Error == nil {
		return ""
	}
	return m.Error.Code
}

// ErrorMessage 返回错误信息，无错误时返回空串。
func (m *Message) ErrorMessage() string {
	if m.Error == nil {
		return ""
	}
	return m.Error.Message
}

// StringData 读取 data 中的字符串字段；不存在或类型不符返回空串。
func (m *Message) StringData(key string) string {
	if m.Data == nil {
		return ""
	}
	s, _ := m.Data[key].(string)
	return s
}

// NRGBAToRGB 将 *image.NRGBA 转为行优先 RGB888（每像素 3 字节，无 alpha）。
// 使用 img.Stride 遍历，正确处理 stride 与宽不等的非 4 对齐情形。
func NRGBAToRGB(img *image.NRGBA) (rgb []byte, w int, h int) {
	w = img.Bounds().Dx()
	h = img.Bounds().Dy()
	rgb = make([]byte, w*h*3)
	stride := img.Stride
	pix := img.Pix
	for y := 0; y < h; y++ {
		src := pix[y*stride : y*stride+w*4]
		dst := rgb[y*w*3 : (y+1)*w*3]
		for x := 0; x < w; x++ {
			dst[x*3] = src[x*4]
			dst[x*3+1] = src[x*4+1]
			dst[x*3+2] = src[x*4+2]
		}
	}
	return rgb, w, h
}

// RGBToNRGBA 将行优先 RGB888 转为 *image.NRGBA（alpha 置 255）。
// 结果 stride 恒等于 w*4。
func RGBToNRGBA(rgb []byte, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	stride := img.Stride
	for y := 0; y < h; y++ {
		src := rgb[y*w*3 : (y+1)*w*3]
		dst := img.Pix[y*stride : y*stride+w*4]
		for x := 0; x < w; x++ {
			dst[x*4] = src[x*3]
			dst[x*4+1] = src[x*3+1]
			dst[x*4+2] = src[x*3+2]
			dst[x*4+3] = 255
		}
	}
	return img
}
