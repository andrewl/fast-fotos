package objects

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
	xdraw "golang.org/x/image/draw"
)

// modelType identifies whether the ONNX model uses SSD or YOLO format.
type modelType int

const (
	modelTypeSSD modelType = iota
	modelTypeYOLO
)

const (
	inputSizeSSD      = 400
	inputSizeYOLOv8   = 640
	confidenceCutoff  = 0.45
)

// Detector identifies a small set of objects in an image.
type Detector interface {
	Detect(context.Context, []byte) ([]string, error)
}

// ONNXDetector runs the object detection model with ONNX Runtime.
type ONNXDetector struct {
	session     *ort.DynamicAdvancedSession
	modelType   modelType
	inputName   string
	outputNames []string
	mu          sync.Mutex
}

// LoadONNXDetector inspects the specified directory for ONNX Runtime dynamic libraries
// and object detection model files (.onnx), initializing a new ONNXDetector session.
func LoadONNXDetector(directory string) (*ONNXDetector, error) {
	runtimePath, err := filepath.Glob(filepath.Join(directory, "libonnxruntime.so.*"))
	if err != nil {
		return nil, fmt.Errorf("find ONNX Runtime library in %s: %w", directory, err)
	}
	if len(runtimePath) == 0 {
		return nil, fmt.Errorf("ONNX Runtime library is missing from %s", directory)
	}
	ort.SetSharedLibraryPath(runtimePath[0])
	if !ort.IsInitialized() {
		if err := ort.InitializeEnvironment(ort.WithLogLevelError()); err != nil {
			return nil, fmt.Errorf("initialize ONNX Runtime: %w", err)
		}
	}

	yoloFiles, _ := filepath.Glob(filepath.Join(directory, "yolo*.onnx"))
	var modelPath string
	var mType modelType
	if len(yoloFiles) > 0 {
		modelPath = yoloFiles[0]
		mType = modelTypeYOLO
	} else {
		modelPath = filepath.Join(directory, "ssd_mobilenet_v1_12.onnx")
		mType = modelTypeSSD
	}

	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("inspect object model: %w", err)
	}
	if len(inputs) == 0 || len(outputs) == 0 {
		return nil, fmt.Errorf("object model in %s has no inputs or outputs", modelPath)
	}

	var outputNames []string
	if mType == modelTypeYOLO {
		outputNames = []string{outputs[0].Name}
	} else {
		if len(inputs) != 1 || len(outputs) != 4 {
			return nil, fmt.Errorf("object model has %d inputs and %d outputs, want 1 and 4", len(inputs), len(outputs))
		}
		outputNames = []string{"detection_boxes", "detection_classes", "detection_scores", "num_detections"}
		for _, name := range outputNames {
			found := false
			for _, output := range outputs {
				if output.Name == name {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("object model output %q is missing", name)
			}
		}
	}

	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{inputs[0].Name}, outputNames, nil)
	if err != nil {
		return nil, fmt.Errorf("load object model: %w", err)
	}
	return &ONNXDetector{session: session, modelType: mType, inputName: inputs[0].Name, outputNames: outputNames}, nil
}

// Close releases the underlying ONNX Runtime session resources.
func (d *ONNXDetector) Close() error {
	return d.session.Destroy()
}

// Detect decodes raw image bytes and runs object detection using the loaded ONNX model.
func (d *ONNXDetector) Detect(ctx context.Context, data []byte) ([]string, error) {
	if d.modelType == modelTypeYOLO {
		return d.detectYOLO(ctx, data)
	}
	return d.detectSSD(ctx, data)
}

// detectYOLO executes YOLO object detection inference and processes output anchor predictions.
func (d *ONNXDetector) detectYOLO(ctx context.Context, data []byte) ([]string, error) {
	inputFloats, err := yoloImageInput(data)
	if err != nil {
		return nil, err
	}
	tensor, err := ort.NewTensor(ort.NewShape(1, 3, inputSizeYOLOv8, inputSizeYOLOv8), inputFloats)
	if err != nil {
		return nil, fmt.Errorf("create YOLOv8 input tensor: %w", err)
	}
	defer tensor.Destroy()

	outputs := make([]ort.Value, 1)
	d.mu.Lock()
	err = d.session.Run([]ort.Value{tensor}, outputs)
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("run YOLOv8 model: %w", err)
	}
	defer func() {
		if outputs[0] != nil {
			_ = outputs[0].Destroy()
		}
	}()

	outputTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("YOLOv8 output has unexpected type %T", outputs[0])
	}
	dataOut := outputTensor.GetData()
	shape := outputTensor.GetShape()

	found := make(map[string]bool)
	var result []string

	// Shape is typically [1, 84, 8400] or [1, 8400, 84]
	if len(shape) == 3 && shape[1] == 84 {
		numAnchors := int(shape[2])
		for c := 0; c < numAnchors; c++ {
			var maxScore float32 = 0
			var maxClass int = -1
			for cls := 0; cls < 80; cls++ {
				score := dataOut[(4+cls)*numAnchors+c]
				if score > maxScore {
					maxScore = score
					maxClass = cls
				}
			}
			if maxScore >= confidenceCutoff && maxClass >= 0 && maxClass < len(cocoLabelsYOLO) {
				label := cocoLabelsYOLO[maxClass]
				if label != "" && !found[label] {
					found[label] = true
					result = append(result, label)
				}
			}
		}
	} else if len(shape) == 3 && shape[2] == 84 {
		numAnchors := int(shape[1])
		for c := 0; c < numAnchors; c++ {
			var maxScore float32 = 0
			var maxClass int = -1
			for cls := 0; cls < 80; cls++ {
				score := dataOut[c*84+4+cls]
				if score > maxScore {
					maxScore = score
					maxClass = cls
				}
			}
			if maxScore >= confidenceCutoff && maxClass >= 0 && maxClass < len(cocoLabelsYOLO) {
				label := cocoLabelsYOLO[maxClass]
				if label != "" && !found[label] {
					found[label] = true
					result = append(result, label)
				}
			}
		}
	}

	return result, nil
}

// detectSSD executes SSD MobileNet object detection inference and processes output tensors.
func (d *ONNXDetector) detectSSD(ctx context.Context, data []byte) ([]string, error) {
	input, err := ssdImageInput(data)
	if err != nil {
		return nil, err
	}
	tensor, err := ort.NewTensor(ort.NewShape(1, inputSizeSSD, inputSizeSSD, 3), input)
	if err != nil {
		return nil, fmt.Errorf("create object input: %w", err)
	}
	defer tensor.Destroy()
	outputs := make([]ort.Value, 4)
	d.mu.Lock()
	err = d.session.Run([]ort.Value{tensor}, outputs)
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("run object model: %w", err)
	}
	defer func() {
		for _, output := range outputs {
			if output != nil {
				_ = output.Destroy()
			}
		}
	}()
	labels, err := labelData(outputs[1])
	if err != nil {
		return nil, err
	}
	scores, err := floatData(outputs[2])
	if err != nil {
		return nil, err
	}
	numDetections, err := floatData(outputs[3])
	if err != nil {
		return nil, err
	}
	found := make(map[string]bool)
	var result []string
	limit := len(scores)
	if len(numDetections) > 0 && int(numDetections[0]) < limit {
		limit = int(numDetections[0])
	}
	for index := 0; index < limit; index++ {
		score := scores[index]
		if score < confidenceCutoff || index >= len(labels) {
			continue
		}
		label := cocoLabel(labels[index])
		if label != "" && !found[label] {
			found[label] = true
			result = append(result, label)
		}
	}
	return result, nil
}

// labelData extracts classification label indices from an ONNX output tensor.
func labelData(output ort.Value) ([]int, error) {
	switch labels := output.(type) {
	case *ort.Tensor[int64]:
		data := labels.GetData()
		result := make([]int, len(data))
		for index, label := range data {
			result[index] = int(label)
		}
		return result, nil
	case *ort.Tensor[int32]:
		data := labels.GetData()
		result := make([]int, len(data))
		for index, label := range data {
			result[index] = int(label)
		}
		return result, nil
	case *ort.Tensor[float32]:
		data := labels.GetData()
		result := make([]int, len(data))
		for index, label := range data {
			result[index] = int(label)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("object model labels output has unexpected type %T", output)
	}
}

// floatData extracts a slice of float32 values from an ONNX output tensor.
func floatData(output ort.Value) ([]float32, error) {
	if values, ok := output.(*ort.Tensor[float32]); ok {
		return values.GetData(), nil
	}
	return nil, fmt.Errorf("object model numeric output has unexpected type %T", output)
}

// ssdImageInput pre-processes image bytes into padded NHWC uint8 tensor data for SSD MobileNet.
func ssdImageInput(data []byte) ([]uint8, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode thumbnail: %w", err)
	}
	bounds := source.Bounds()
	scale := min(float64(inputSizeSSD)/float64(bounds.Dx()), float64(inputSizeSSD)/float64(bounds.Dy()))
	width := max(1, int(float64(bounds.Dx())*scale))
	height := max(1, int(float64(bounds.Dy())*scale))
	resized := image.NewRGBA(image.Rect(0, 0, inputSizeSSD, inputSizeSSD))
	draw.Draw(resized, resized.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	destination := image.Rect((inputSizeSSD-width)/2, (inputSizeSSD-height)/2, (inputSizeSSD-width)/2+width, (inputSizeSSD-height)/2+height)
	xdraw.ApproxBiLinear.Scale(resized, destination, source, bounds, draw.Over, nil)
	for y := 0; y < inputSizeSSD; y++ {
		for x := 0; x < inputSizeSSD; x++ {
			resized.SetRGBA(x, y, color.RGBAModel.Convert(resized.At(x, y)).(color.RGBA))
		}
	}
	dataOut := make([]uint8, 3*inputSizeSSD*inputSizeSSD)
	for y := 0; y < inputSizeSSD; y++ {
		for x := 0; x < inputSizeSSD; x++ {
			pixel := resized.RGBAAt(x, y)
			offset := (y*inputSizeSSD + x) * 3
			dataOut[offset] = pixel.R
			dataOut[offset+1] = pixel.G
			dataOut[offset+2] = pixel.B
		}
	}
	return dataOut, nil
}

// yoloImageInput pre-processes image bytes into padded NCHW float32 tensor data normalized to [0, 1] for YOLO.
func yoloImageInput(data []byte) ([]float32, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode thumbnail: %w", err)
	}
	bounds := source.Bounds()
	scale := min(float64(inputSizeYOLOv8)/float64(bounds.Dx()), float64(inputSizeYOLOv8)/float64(bounds.Dy()))
	width := max(1, int(float64(bounds.Dx())*scale))
	height := max(1, int(float64(bounds.Dy())*scale))
	resized := image.NewRGBA(image.Rect(0, 0, inputSizeYOLOv8, inputSizeYOLOv8))
	draw.Draw(resized, resized.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	destination := image.Rect((inputSizeYOLOv8-width)/2, (inputSizeYOLOv8-height)/2, (inputSizeYOLOv8-width)/2+width, (inputSizeYOLOv8-height)/2+height)
	xdraw.ApproxBiLinear.Scale(resized, destination, source, bounds, draw.Over, nil)

	planeSize := inputSizeYOLOv8 * inputSizeYOLOv8
	dataOut := make([]float32, 3*planeSize)
	for y := 0; y < inputSizeYOLOv8; y++ {
		for x := 0; x < inputSizeYOLOv8; x++ {
			pixel := resized.RGBAAt(x, y)
			idx := y*inputSizeYOLOv8 + x
			dataOut[idx] = float32(pixel.R) / 255.0
			dataOut[planeSize+idx] = float32(pixel.G) / 255.0
			dataOut[2*planeSize+idx] = float32(pixel.B) / 255.0
		}
	}
	return dataOut, nil
}

var cocoLabels = []string{"", "Person", "Bicycle", "Car", "Motorcycle", "Airplane", "Bus", "Train", "Truck", "Boat", "Traffic light", "Fire hydrant", "Stop sign", "Parking meter", "Bench", "Bird", "Cat", "Dog", "Horse", "Sheep", "Cow", "Elephant", "Bear", "Zebra", "Giraffe", "Backpack", "Umbrella", "Handbag", "Tie", "Suitcase", "Frisbee", "Skis", "Snowboard", "Sports ball", "Kite", "Baseball bat", "Baseball glove", "Skateboard", "Surfboard", "Tennis racket", "Bottle", "Wine glass", "Cup", "Fork", "Knife", "Spoon", "Bowl", "Banana", "Apple", "Sandwich", "Orange", "Broccoli", "Carrot", "Hot dog", "Pizza", "Donut", "Cake", "Chair", "Couch", "Potted plant", "Bed", "Dining table", "Toilet", "TV", "Laptop", "Mouse", "Remote", "Keyboard", "Cell phone", "Microwave", "Oven", "Toaster", "Sink", "Refrigerator", "Book", "Clock", "Vase", "Scissors", "Teddy bear", "Hair drier", "Toothbrush"}

var cocoLabelsYOLO = []string{
	"Person", "Bicycle", "Car", "Motorcycle", "Airplane", "Bus", "Train", "Truck", "Boat", "Traffic light",
	"Fire hydrant", "Stop sign", "Parking meter", "Bench", "Bird", "Cat", "Dog", "Horse", "Sheep", "Cow",
	"Elephant", "Bear", "Zebra", "Giraffe", "Backpack", "Umbrella", "Handbag", "Tie", "Suitcase", "Frisbee",
	"Skis", "Snowboard", "Sports ball", "Kite", "Baseball bat", "Baseball glove", "Skateboard", "Surfboard", "Tennis racket",
	"Bottle", "Wine glass", "Cup", "Fork", "Knife", "Spoon", "Bowl", "Banana", "Apple", "Sandwich",
	"Orange", "Broccoli", "Carrot", "Hot dog", "Pizza", "Donut", "Cake", "Chair", "Couch", "Potted plant",
	"Bed", "Dining table", "Toilet", "TV", "Laptop", "Mouse", "Remote", "Keyboard", "Cell phone", "Microwave",
	"Oven", "Toaster", "Sink", "Refrigerator", "Book", "Clock", "Vase", "Scissors", "Teddy bear", "Hair drier",
	"Toothbrush",
}

// cocoLabel returns the string label for a 1-based COCO class index.
func cocoLabel(index int) string {
	if index < 0 || index >= len(cocoLabels) {
		return ""
	}
	return cocoLabels[index]
}
