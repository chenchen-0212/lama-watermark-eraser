package inpaint

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// ModelDim 模型固定输入边长（Carve/LaMa-ONNX lama_fp32 为 512x512）。
const ModelDim = 512

var envInitialized = false

// Engine 封装 ONNX LaMa 会话；固定形状张量预分配并跨图复用。
type Engine struct {
	sess       *ort.AdvancedSession
	imgTensor  *ort.Tensor[float32]
	maskTensor *ort.Tensor[float32]
	outTensor  *ort.Tensor[float32]
	imgData    []float32 // [1,3,512,512] RGB 0..1
	maskData   []float32 // [1,1,512,512] {0,1}
	outData    []float32 // [1,3,512,512]
	outFactor  float32   // 输出转 0..255 的乘子：255(0..1 输出) 或 1(0..255 输出)
}

// NewEngine 初始化 onnxruntime 环境并加载 LaMa ONNX 模型。
func NewEngine(modelPath, dllPath string) (*Engine, error) {
	ort.SetSharedLibraryPath(dllPath)
	if !envInitialized {
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("初始化 onnxruntime 失败: %w", err)
		}
		envInitialized = true
	}

	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("读取模型信息失败: %w", err)
	}
	if len(inputs) != 2 || len(outputs) < 1 {
		return nil, fmt.Errorf("意外的模型结构: %d 输入 / %d 输出", len(inputs), len(outputs))
	}
	var imgName, maskName string
	for _, in := range inputs {
		if len(in.Dimensions) == 4 && in.Dimensions[1] == 3 {
			imgName = in.Name
		} else {
			maskName = in.Name
		}
	}
	if imgName == "" || maskName == "" {
		imgName, maskName = inputs[0].Name, inputs[1].Name
	}
	outName := outputs[0].Name

	e := &Engine{
		imgData:  make([]float32, 3*ModelDim*ModelDim),
		maskData: make([]float32, ModelDim*ModelDim),
	}
	if e.imgTensor, err = ort.NewTensor(ort.NewShape(1, 3, ModelDim, ModelDim), e.imgData); err != nil {
		return nil, fmt.Errorf("创建图像张量失败: %w", err)
	}
	if e.maskTensor, err = ort.NewTensor(ort.NewShape(1, 1, ModelDim, ModelDim), e.maskData); err != nil {
		return nil, fmt.Errorf("创建掩膜张量失败: %w", err)
	}
	if e.outTensor, err = ort.NewEmptyTensor[float32](ort.NewShape(1, 3, ModelDim, ModelDim)); err != nil {
		return nil, fmt.Errorf("创建输出张量失败: %w", err)
	}
	// 统一改为引用张量底层数据，确保读写指向 ORT 实际使用的内存
	e.imgData = e.imgTensor.GetData()
	e.maskData = e.maskTensor.GetData()
	e.outData = e.outTensor.GetData()
	if e.sess, err = ort.NewAdvancedSession(modelPath,
		[]string{imgName, maskName}, []string{outName},
		[]ort.Value{e.imgTensor, e.maskTensor}, []ort.Value{e.outTensor}, nil); err != nil {
		return nil, fmt.Errorf("创建会话失败(模型: %s): %w", modelPath, err)
	}

	// 输出范围探测 + 模型预热：白色画布 + 空掩膜
	for i := range e.imgData {
		e.imgData[i] = 1.0
	}
	for i := range e.maskData {
		e.maskData[i] = 0
	}
	if err := e.sess.Run(); err != nil {
		return nil, fmt.Errorf("模型推理冒烟失败: %w", err)
	}
 vmax := float32(0)
	for _, v := range e.outData {
		if v > vmax {
			vmax = v
		}
	}
	if vmax > 2.0 {
		e.outFactor = 1.0 // 输出 0..255
	} else {
		e.outFactor = 255.0 // 输出 0..1
	}
	return e, nil
}

// Run 执行一次推理（调用前先填充张量数据）。
func (e *Engine) Run() error { return e.sess.Run() }

// OutFactor 输出数值转 0..255 的乘子。
func (e *Engine) OutFactor() float32 { return e.outFactor }

// Close 释放会话与张量（环境保持初始化以便复用）。
func (e *Engine) Close() {
	if e.sess != nil {
		e.sess.Destroy()
		e.sess = nil
	}
	if e.imgTensor != nil {
		e.imgTensor.Destroy()
		e.imgTensor = nil
	}
	if e.maskTensor != nil {
		e.maskTensor.Destroy()
		e.maskTensor = nil
	}
	if e.outTensor != nil {
		e.outTensor.Destroy()
		e.outTensor = nil
	}
}
