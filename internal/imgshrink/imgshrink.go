// Package imgshrink 把「内联在 OpenAI 请求里的 base64 图片」按需缩图 / 转 JPEG。
//
// 背景（2026-11-03 实测）：华为云 CodeArts 的媒体转换对单张图片有硬上限 ——
// 请求体约 4.3MB / 图片文件约 3.2MB 之内正常，再大就回
// `InferHub.ModelArts.81029.400 Media conversion failed: downstream returned 413`。
// 这个上限在**上游**，网关自己放宽 body 上限并不能绕过，所以只能把图片压小再发。
//
// 设计约束：
//   - 只用标准库（image/png、image/jpeg、复用自带的双线性缩放），项目零外部依赖；
//   - **只在需要时动手**：请求体没超阈值、每张图也没超预算时原样放行，不动像素；
//   - 透明像素合成到白底再编码 JPEG（否则 UI 截图会变黑块）；
//   - webp / gif 等标准库解码不了的格式**原样放过**，宁可不压也不要把图弄坏。
package imgshrink

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码器（Decode/DecodeConfig 依赖注册表）
	"math"
	"strings"
)

// Options 压缩参数（由 config 的 image_shrink 段注入）。
type Options struct {
	// TargetBytes 单张图片编码后的目标上限（字节）。
	TargetBytes int
	// MaxDimension 长边像素上限；超过则等比缩小。
	MaxDimension int
	// Quality 初始 JPEG 质量（后续按需递减到 minQuality）。
	Quality int
}

const (
	minQuality   = 55
	minDimension = 160
)

// maxDecodePixels 单张图解码像素上限。超过就直接放过（不压、不改）：
// 一张 10000×10000 的 PNG 解成 RGBA 要 ~400MB，宁可不压也不能把面板 OOM 掉。
// 变量而非常量：测试里会调小来验证这条护栏。
var maxDecodePixels = 40_000_000

// ErrTooLargeToProcess 表示图片像素数超过护栏，调用方应当原样转发。
var ErrTooLargeToProcess = errors.New("imgshrink: 图片像素数过大，跳过处理")

// DefaultOptions 与 config 默认值保持一致：单张 ≤2MB、长边 ≤2048、质量 85。
func DefaultOptions() Options {
	return Options{TargetBytes: 2 << 20, MaxDimension: 2048, Quality: 85}
}

func (o Options) normalized() Options {
	d := DefaultOptions()
	if o.TargetBytes <= 0 {
		o.TargetBytes = d.TargetBytes
	}
	if o.MaxDimension <= 0 {
		o.MaxDimension = d.MaxDimension
	}
	if o.Quality <= 0 || o.Quality > 100 {
		o.Quality = d.Quality
	}
	return o
}

// Result 描述一次压缩的结果（未改动时 Changed=false）。
type Result struct {
	Changed   bool
	MediaType string
	Before    int
	After     int
	Width     int
	Height    int
	OutWidth  int
	OutHeight int
}

// ErrUnsupported 表示格式不支持（调用方应当原样放行）。
var ErrUnsupported = errors.New("imgshrink: unsupported image format")

// IsDataURL 判断是否 `data:image/...;base64,...`，并取回媒体类型与 base64 载荷。
func IsDataURL(raw string) (mediaType, payload string, ok bool) {
	if !strings.HasPrefix(raw, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := raw[len("data:"):comma]
	if !strings.Contains(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	return mediaType, raw[comma+1:], true
}

// ShrinkDataURL 按需压缩一个 data URL 图片。
//
// 返回 (新 data URL, 结果, error)。当图片本来就够小、或格式不支持时，
// 返回原字符串且 Result.Changed=false（err 为 nil 或 ErrUnsupported）。
func ShrinkDataURL(raw string, opt Options) (string, Result, error) {
	opt = opt.normalized()
	mediaType, payload, ok := IsDataURL(raw)
	if !ok {
		return raw, Result{}, ErrUnsupported
	}
	// 只碰标准库解得了、且上游确定接受的两种格式。
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		return raw, Result{MediaType: mediaType}, ErrUnsupported
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// 宽松一点：兼容不带 padding 的写法。
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil {
			return raw, Result{MediaType: mediaType}, fmt.Errorf("decode base64: %w", err)
		}
	}
	res := Result{MediaType: mediaType, Before: len(decoded), After: len(decoded)}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		return raw, res, fmt.Errorf("decode image header: %w", err)
	}
	res.Width, res.Height = cfg.Width, cfg.Height
	res.OutWidth, res.OutHeight = cfg.Width, cfg.Height

	longSide := cfg.Width
	if cfg.Height > longSide {
		longSide = cfg.Height
	}
	overBytes := len(decoded) > opt.TargetBytes
	overDims := longSide > opt.MaxDimension
	if !overBytes && !overDims {
		return raw, res, nil // 本来合格：一个字节都不动
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(maxDecodePixels) {
		return raw, res, ErrTooLargeToProcess
	}

	src, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return raw, res, fmt.Errorf("decode image: %w", err)
	}

	// JPEG 的 EXIF 方向必须自己转过来：Go 的解码器不认 EXIF，
	// 手机竖拍的照片（orientation=6）直接重编码会躺倒 90°，模型就会看歪。
	srcW, srcH := cfg.Width, cfg.Height
	if mediaType == "image/jpeg" {
		if o := jpegOrientation(decoded); o != 1 {
			src, srcW, srcH = applyOrientation(src, o)
		}
	}

	w, h := srcW, srcH
	if longest := maxInt(srcW, srcH); longest > opt.MaxDimension {
		scale := float64(opt.MaxDimension) / float64(longest)
		w, h = int(float64(w)*scale), int(float64(h)*scale)
	}

	// 先按当前尺寸试不同质量；都不够小就把尺寸打八五折再来一轮。
	// 全程记住「见过的最小结果」，即便最终没压进目标也不会比原来更大。
	var best []byte
	var bestW, bestH int
	found := false
	qualityFloor := min(opt.Quality, minQuality)
	for round := 0; round < 6; round++ {
		// 第 0 轮必须跑：如果 max_dimension 本身就小于 minDimension（例如设成 300 缩竖图），
		// 先 break 会导致"一张都编不出来"直接失败。minDimension 只用于限制后续继续缩。
		if round > 0 && (w < minDimension || h < minDimension) {
			break
		}
		if w < 1 || h < 1 {
			break
		}
		scaled := scaleRGBA(src, w, h)
		hit := false
		for q := opt.Quality; q >= qualityFloor; q -= 10 {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: q}); err != nil {
				return raw, res, fmt.Errorf("encode jpeg: %w", err)
			}
			if !found || buf.Len() < len(best) {
				best, bestW, bestH = append([]byte(nil), buf.Bytes()...), w, h
				found = true
			}
			if buf.Len() <= opt.TargetBytes {
				hit = true
				break
			}
		}
		if hit {
			break
		}
		w, h = int(float64(w)*0.85), int(float64(h)*0.85)
	}
	if !found {
		return raw, res, errors.New("imgshrink: 无法把图片压到目标体积内")
	}
	if len(best) >= len(decoded) {
		return raw, res, nil // 压缩没收益（例如本身就是高质量小 JPEG）→ 原样
	}
	out := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(best)
	res.Changed = true
	res.After = len(best)
	res.OutWidth, res.OutHeight = bestW, bestH
	return out, res, nil
}

// scaleRGBA 双线性缩放到 w×h，并把透明像素合成到白底（JPEG 没有 alpha 通道，
// UI 截图的透明区域若直接编码会变成黑块）。
func scaleRGBA(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 || w <= 0 || h <= 0 {
		return dst
	}
	sx := float64(sw) / float64(w)
	sy := float64(sh) / float64(h)
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*sy - 0.5
		y0 := int(math.Floor(fy))
		wy := fy - float64(y0)
		if y0 < 0 {
			y0, wy = 0, 0
		}
		y1 := y0 + 1
		if y1 > sh-1 {
			y1 = sh - 1
		}
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*sx - 0.5
			x0 := int(math.Floor(fx))
			wx := fx - float64(x0)
			if x0 < 0 {
				x0, wx = 0, 0
			}
			x1 := x0 + 1
			if x1 > sw-1 {
				x1 = sw - 1
			}
			// 四个角按预乘 alpha 采样（RGBA() 返回的本来就是预乘值）。
			r00, g00, b00, a00 := src.At(b.Min.X+x0, b.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := src.At(b.Min.X+x1, b.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := src.At(b.Min.X+x0, b.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := src.At(b.Min.X+x1, b.Min.Y+y1).RGBA()

			lerp := func(v00, v10, v01, v11 uint32) float64 {
				top := float64(v00)*(1-wx) + float64(v10)*wx
				bot := float64(v01)*(1-wx) + float64(v11)*wx
				return (top*(1-wy) + bot*wy) / 65535.0
			}
			r := lerp(r00, r10, r01, r11)
			g := lerp(g00, g10, g01, g11)
			bl := lerp(b00, b10, b01, b11)
			a := lerp(a00, a10, a01, a11)
			// 预乘值直接叠白底：out = premult + (1-a)*white
			inv := 1 - a
			r, g, bl = r+inv, g+inv, bl+inv
			i := dst.PixOffset(x, y)
			dst.Pix[i] = clamp8(r)
			dst.Pix[i+1] = clamp8(g)
			dst.Pix[i+2] = clamp8(bl)
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 1:
		return 255
	default:
		return uint8(v*255 + 0.5)
	}
}

// Policy 是一次请求的压缩策略（对应 config 的 image_shrink 段）。
// Policy 是缩图参数。**没有 Enabled**：开关只在配置/快照那一层判断一次
// —— 两处各存一个"是否启用"很容易漏设一个，结果就是静默不生效。
type Policy struct {
	// TriggerBytes 请求体超过它才考虑压缩（避免给小请求白付 CPU）。
	TriggerBytes int64
	// TargetImageBytes 单张图片编码后的目标上限。
	TargetImageBytes int
	// TargetTotalBytes 本请求内所有图片合计的目标上限。
	TargetTotalBytes int
	// MaxDimension 长边像素上限。
	MaxDimension int
	// Quality 初始 JPEG 质量。
	Quality int
}

// DefaultPolicy 与 config 默认值一致：启用、4MB 触发、单张 2MB、合计 4MB、长边 2048、质量 85。
func DefaultPolicy() Policy {
	return Policy{
		TriggerBytes:     4 << 20,
		TargetImageBytes: 2 << 20,
		TargetTotalBytes: 4 << 20,
		MaxDimension:     2048,
		Quality:          85,
	}
}

// Normalized 补默认值（整型零值视为未设置）。
func (p Policy) Normalized() Policy {
	d := DefaultPolicy()
	if p.TriggerBytes <= 0 {
		p.TriggerBytes = d.TriggerBytes
	}
	if p.TargetImageBytes <= 0 {
		p.TargetImageBytes = d.TargetImageBytes
	}
	if p.TargetTotalBytes <= 0 {
		p.TargetTotalBytes = d.TargetTotalBytes
	}
	if p.MaxDimension <= 0 {
		p.MaxDimension = d.MaxDimension
	}
	if p.Quality <= 0 || p.Quality > 100 {
		p.Quality = d.Quality
	}
	return p
}

// Options 生成本次压缩用的 Options。
func (p Policy) Options() Options {
	return Options{
		TargetBytes:  p.TargetImageBytes,
		MaxDimension: p.MaxDimension,
		Quality:      p.Quality,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// jpegOrientation 从 JPEG 的 APP1(EXIF) 里读 Orientation 标签（0x0112）。
// 读不到／格式不认识一律返回 1（不旋转），绝不猜。
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		// 独立标记（无长度字段）
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // 到扫描数据/结束，后面没有元数据了
			return 1
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+segLen]
		if marker == 0xE1 && len(seg) >= 6 && seg[0] == 'E' && seg[1] == 'x' && seg[2] == 'i' && seg[3] == 'f' && seg[4] == 0 && seg[5] == 0 {
			return parseExifOrientation(seg[6:])
		}
		i += 2 + segLen
	}
	return 1
}

// parseExifOrientation 解析 TIFF 头里的 IFD0，找 0x0112 的 SHORT 值。
func parseExifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifd := int(bo.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	count := int(bo.Uint16(tiff[ifd : ifd+2]))
	off := ifd + 2
	for k := 0; k < count; k++ {
		if off+12 > len(tiff) {
			return 1
		}
		tag := bo.Uint16(tiff[off : off+2])
		typ := bo.Uint16(tiff[off+2 : off+4])
		if tag == 0x0112 && typ == 3 {
			if v := int(bo.Uint16(tiff[off+8 : off+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
		off += 12
	}
	return 1
}

// applyOrientation 按 EXIF 方向把图像摆正，返回摆正后的图像与新宽高。
func applyOrientation(src image.Image, orientation int) (image.Image, int, int) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || orientation <= 1 || orientation > 8 {
		return src, w, h
	}
	outW, outH := w, h
	if orientation >= 5 {
		outW, outH = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x, y
			switch orientation {
			case 2: // 水平镜像
				dx = w - 1 - x
			case 3: // 180°
				dx, dy = w-1-x, h-1-y
			case 4: // 垂直镜像
				dy = h - 1 - y
			case 5: // 主对角线转置
				dx, dy = y, x
			case 6: // 顺时针 90°
				dx, dy = h-1-y, x
			case 7: // 副对角线转置
				dx, dy = h-1-y, w-1-x
			case 8: // 逆时针 90°
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst, outW, outH
}
