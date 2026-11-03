package imgshrink

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// 生成噪点 PNG（噪点才压不小，用它模拟「真·大截图」）。
func noisePNG(t *testing.T, w, h int, seed int64) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(seed))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func dataURL(mediaType string, raw []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func decodePayload(t *testing.T, dataURL string) []byte {
	t.Helper()
	_, payload, ok := IsDataURL(dataURL)
	if !ok {
		t.Fatalf("不是 data URL: %.60s", dataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return raw
}

// 大噪点图必须被压到目标体积内，并且变成 JPEG。
func TestShrinkOversizeNoiseToJPEG(t *testing.T) {
	raw := noisePNG(t, 700, 700, 11)
	if len(raw) < 500<<10 {
		t.Fatalf("样本太小，测不到压缩：%d", len(raw))
	}
	out, res, err := ShrinkDataURL(dataURL("image/png", raw), Options{TargetBytes: 200 << 10, MaxDimension: 2048, Quality: 85})
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if !res.Changed {
		t.Fatal("大图应当被压缩")
	}
	if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
		t.Fatalf("应当转成 JPEG，got=%.40s", out)
	}
	got := decodePayload(t, out)
	if len(got) > 200<<10 {
		t.Fatalf("压缩后仍有 %d 字节，超过目标 200KB", len(got))
	}
	if len(got) >= res.Before {
		t.Fatalf("压缩没变小：before=%d after=%d", res.Before, len(got))
	}
	if _, _, err := image.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("输出不是可解码图片: %v", err)
	}
}

func TestShrinkHonoursLowJPEGQuality(t *testing.T) {
	raw := noisePNG(t, 400, 400, 42)
	for _, quality := range []int{1, 50, 54} {
		t.Run(strconv.Itoa(quality), func(t *testing.T) {
			out, res, err := ShrinkDataURL(dataURL("image/png", raw), Options{
				TargetBytes: 100 << 10, MaxDimension: 2048, Quality: quality,
			})
			if err != nil {
				t.Fatalf("shrink at quality %d: %v", quality, err)
			}
			if !res.Changed || len(decodePayload(t, out)) > 100<<10 {
				t.Fatalf("low JPEG quality must still shrink to budget: %+v", res)
			}
			if _, _, err := image.Decode(bytes.NewReader(decodePayload(t, out))); err != nil {
				t.Fatalf("decode output: %v", err)
			}
		})
	}
}

// 本来就小的图：一个字节都不动（Changed=false 且字符串原样返回）。
func TestShrinkKeepsSmallImageUntouched(t *testing.T) {
	raw := noisePNG(t, 64, 64, 7)
	in := dataURL("image/png", raw)
	out, res, err := ShrinkDataURL(in, DefaultOptions())
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if res.Changed || out != in {
		t.Fatalf("小图不应被改动：changed=%v len=%d", res.Changed, len(out))
	}
}

// 体积没超预算、但长边超上限的图，也要按长边缩（省视觉 token）。
func TestShrinkLimitsDimension(t *testing.T) {
	raw := noisePNG(t, 1500, 1500, 23)
	if len(raw) > 8<<20 {
		t.Fatalf("样本不该超字节预算，否则测的就不是「仅超长边」：%d", len(raw))
	}
	_, res, err := ShrinkDataURL(dataURL("image/png", raw), Options{TargetBytes: 8 << 20, MaxDimension: 512, Quality: 85})
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if !res.Changed {
		t.Fatal("超长边必须被缩")
	}
	if res.OutWidth > 512 || res.OutHeight > 512 {
		t.Fatalf("长边没缩到位：%dx%d", res.OutWidth, res.OutHeight)
	}
}

// 体积本来就小于目标、尺寸也合规：即使 PNG 换成 JPEG 会更大，也坚决不换。
func TestShrinkSkipsWhenNoByteGain(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 120, 255}) // 极强规律性，PNG 极小
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	in := dataURL("image/png", buf.Bytes())
	out, res, err := ShrinkDataURL(in, Options{TargetBytes: 4 << 20, MaxDimension: 2048, Quality: 85})
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if res.Changed || out != in {
		t.Fatalf("没体积收益就不该换图：changed=%v before=%d", res.Changed, res.Before)
	}
}

// 透明区域必须合成到白底，绝不能变成黑块（JPEG 无 alpha）。
func TestShrinkCompositesAlphaOntoWhite(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	rnd := rand.New(rand.NewSource(31))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			if x < 200 {
				img.Set(x, y, color.RGBA{0, 0, 0, 0}) // 全透明
			} else {
				// 右半边用噪点撑体积，保证 PNG 明显大于 JPEG，才测得到「换图」路径
				img.Set(x, y, color.RGBA{uint8(150 + rnd.Intn(100)), uint8(rnd.Intn(80)), uint8(rnd.Intn(80)), 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	out, res, err := ShrinkDataURL(dataURL("image/png", buf.Bytes()), Options{TargetBytes: 60 << 10, MaxDimension: 400, Quality: 85})
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if !res.Changed {
		t.Fatal("样本应当被压缩")
	}
	im, _, err := image.Decode(bytes.NewReader(decodePayload(t, out)))
	if err != nil {
		t.Fatalf("decode out: %v", err)
	}
	r, g, b, _ := im.At(20, im.Bounds().Dy()/2).RGBA()
	if r < 200*257 || g < 200*257 || b < 200*257 {
		t.Fatalf("透明区没有合成到白底：rgb=%d,%d,%d", r>>8, g>>8, b>>8)
	}
}

// 解不了的格式（webp）与非 data URL：原样放过，不报错不破坏。
func TestShrinkLeavesUnsupportedAlone(t *testing.T) {
	for _, in := range []string{
		dataURL("image/webp", []byte("not really webp")),
		"https://example.com/a.png",
	} {
		out, res, err := ShrinkDataURL(in, DefaultOptions())
		if out != in || res.Changed {
			t.Fatalf("不支持的输入被改动了：%.40s", in)
		}
		if err != nil && !errors.Is(err, ErrUnsupported) {
			t.Fatalf("应当是 ErrUnsupported，got=%v", err)
		}
	}
}

func TestPolicyDefaults(t *testing.T) {
	p := Policy{}.Normalized()
	if p.TriggerBytes != 4<<20 || p.TargetImageBytes != 2<<20 || p.MaxDimension != 2048 || p.Quality != 85 {
		t.Fatalf("默认策略不对：%+v", p)
	}
}

// 像素数超过护栏的图必须直接放过：解一张 10000×10000 的 PNG 要 ~400MB，宁可不压也不能把面板打爆。
func TestShrinkSkipsPixelsTooLarge(t *testing.T) {
	old := maxDecodePixels
	maxDecodePixels = 100 // 64×64 就有 4096 px，必定超
	defer func() { maxDecodePixels = old }()

	raw := noisePNG(t, 64, 64, 3)
	in := dataURL("image/png", raw)
	out, res, err := ShrinkDataURL(in, Options{TargetBytes: 1 << 10, MaxDimension: 2048, Quality: 85})
	if !errors.Is(err, ErrTooLargeToProcess) {
		t.Fatalf("应当返回 ErrTooLargeToProcess，got=%v", err)
	}
	if out != in || res.Changed {
		t.Fatal("超过护栏的图必须原样放过")
	}
}

// jpegWithOrientation 造一张带 EXIF Orientation 的 JPEG（其余 APP 段沿用编码器输出）。
func jpegWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(int64(w*100 + h)))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
		}
	}
	return jpegImageWithOrientation(t, img, orientation)
}

func jpegImageWithOrientation(t *testing.T, img image.Image, orientation int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	raw := buf.Bytes()

	tiff := new(bytes.Buffer)
	tiff.WriteString("II")
	_ = binary.Write(tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(1))      // entry 数
	_ = binary.Write(tiff, binary.LittleEndian, uint16(0x0112)) // Orientation
	_ = binary.Write(tiff, binary.LittleEndian, uint16(3))      // SHORT
	_ = binary.Write(tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(orientation))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(0)) // 值域补齐
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0)) // 下一个 IFD 偏移

	seg := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	out = append(out, seg...)
	return append(out, raw[2:]...) // 去掉原 SOI，保住其余段
}

func TestJPEGOrientationParsing(t *testing.T) {
	for _, want := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		raw := jpegWithOrientation(t, 40, 20, want)
		if got := jpegOrientation(raw); got != want {
			t.Fatalf("orientation=%d 解析成 %d", want, got)
		}
	}
	// 没有 EXIF 的普通 JPEG 必须当作 1（不旋转），不能猜。
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := jpegOrientation(buf.Bytes()); got != 1 {
		t.Fatalf("无 EXIF 时应为 1，got=%d", got)
	}
}

func TestShrinkHonoursAllJPEGOrientations(t *testing.T) {
	colors := []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 0, 255}}
	img := image.NewRGBA(image.Rect(0, 0, 600, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 600; x++ {
			img.SetRGBA(x, y, colors[(y/200)*2+x/300])
		}
	}
	// Expected displayed quadrants, in reading order, from the EXIF orientation definitions.
	for i, corners := range [][4]int{
		{0, 1, 2, 3}, {1, 0, 3, 2}, {3, 2, 1, 0}, {2, 3, 0, 1},
		{0, 2, 1, 3}, {2, 0, 3, 1}, {3, 1, 2, 0}, {1, 3, 0, 2},
	} {
		orientation := i + 1
		t.Run(strconv.Itoa(orientation), func(t *testing.T) {
			raw := jpegImageWithOrientation(t, img, orientation)
			out, res, err := ShrinkDataURL(dataURL("image/jpeg", raw), Options{
				TargetBytes: 8 << 20, MaxDimension: 300, Quality: 85,
			})
			if err != nil || !res.Changed {
				t.Fatalf("shrink: changed=%v err=%v", res.Changed, err)
			}
			decoded, _, err := image.Decode(bytes.NewReader(decodePayload(t, out)))
			if err != nil {
				t.Fatalf("decode output: %v", err)
			}
			w, h := 300, 200
			if orientation >= 5 {
				w, h = h, w
			}
			if decoded.Bounds().Dx() != w || decoded.Bounds().Dy() != h {
				t.Fatalf("displayed dimensions: got %v want %dx%d", decoded.Bounds(), w, h)
			}
			for corner, index := range corners {
				x, y := (2*(corner%2)+1)*w/4, (2*(corner/2)+1)*h/4
				r, g, b, _ := decoded.At(x, y).RGBA()
				want := colors[index]
				if abs(int(r>>8)-int(want.R)) > 30 || abs(int(g>>8)-int(want.G)) > 30 || abs(int(b>>8)-int(want.B)) > 30 {
					t.Fatalf("quadrant %d: got RGB(%d,%d,%d) want %v", corner, r>>8, g>>8, b>>8, want)
				}
			}
		})
	}
}

// 顺时针 90°：原 (0,0) 落到新图右上角，宽高互换。
func TestApplyOrientationRotates(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})

	out, w, h := applyOrientation(img, 6)
	if w != 20 || h != 40 {
		t.Fatalf("尺寸应互换，got=%dx%d", w, h)
	}
	r, g, b, _ := out.At(19, 0).RGBA()
	if r>>8 < 200 || g>>8 > 60 || b>>8 > 60 {
		t.Fatalf("左上角的红色没有转到右上角：r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

// 端到端：带 EXIF orientation=6 的 JPEG 压缩后尺寸必须是摆正后的（宽高互换）。
func TestShrinkHonoursJPEGOrientation(t *testing.T) {
	raw := jpegWithOrientation(t, 600, 300, 6)
	// 长边上限 300：摆正后长边是 600（竖向），缩到 300 → 期望 150×300。
	// （用长边而不是字节预算来断言，避免字节压缩再把尺寸改小、把断言搞成"跟着实现走"。）
	_, res, err := ShrinkDataURL(dataURL("image/jpeg", raw), Options{TargetBytes: 8 << 20, MaxDimension: 300, Quality: 85})
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if !res.Changed {
		t.Fatal("样本应当被压缩")
	}
	if res.OutHeight <= res.OutWidth {
		t.Fatalf("没有按 EXIF 方向摆正（应为竖图）：out=%dx%d", res.OutWidth, res.OutHeight)
	}
	if abs(res.OutWidth-150) > 2 || abs(res.OutHeight-300) > 2 {
		t.Fatalf("摆正后长边限制不对：out=%dx%d（期望约 150x300）", res.OutWidth, res.OutHeight)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
