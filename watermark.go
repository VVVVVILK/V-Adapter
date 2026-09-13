// watermark.go — 去除生成图右下角的平台水印角标（如 "Qwen"）。
//
// 背景：聊天接口生图兜底返回的是上游官网生成的图，右下角带 "Qwen" 水印
// （用户实测确认；标准生图接口未含此水印，故只处理 chat 链路）。
// 做法：对右下角固定比例区域做多次均值模糊，抹掉角标文字、保留背景纹理。
// 仅支持 PNG/JPEG；webp/gif/bmp 等格式原样返回（不处理，避免解码失败）。
package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// 右下角水印区域比例（相对图宽高）。实测聊天链路水印占右 ~11%、下 ~9%，
// 留足余量保证完整覆盖。
const (
	wmRightRatio  = 0.15
	wmBottomRatio = 0.12
)

// removeWatermark 把图片右下角区域做均值模糊；不支持的格式/解码失败返回原字节。
func removeWatermark(data []byte, ext string) []byte {
	var (
		img   image.Image
		err   error
		isJPG bool
	)
	switch ext {
	case "png":
		img, err = png.Decode(bytes.NewReader(data))
	case "jpg", "jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
		isJPG = true
	default:
		return data
	}
	if err != nil {
		return data
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 64 || h < 64 {
		return data
	}
	x0 := b.Min.X + int(float64(w)*(1-wmRightRatio))
	y0 := b.Min.Y + int(float64(h)*(1-wmBottomRatio))
	x1, y1 := b.Max.X, b.Max.Y
	if x0 >= x1 || y0 >= y1 {
		return data
	}

	// 转 RGBA 以便修改
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)

	// 迭代均值模糊（9x9 邻域，6 轮），把角标文字彻底抹平、背景纹理保留
	for iter := 0; iter < 6; iter++ {
		tmp := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
		draw.Draw(tmp, tmp.Bounds(), rgba, image.Pt(x0, y0), draw.Src)
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				var r, g, bl, a, n int
				for dy := -4; dy <= 4; dy++ {
					for dx := -4; dx <= 4; dx++ {
						nx, ny := x+dx, y+dy
						if nx < x0 {
							nx = x0
						} else if nx >= x1 {
							nx = x1 - 1
						}
						if ny < y0 {
							ny = y0
						} else if ny >= y1 {
							ny = y1 - 1
						}
						c := tmp.At(nx-x0, ny-y0)
						rr, gg, bb, aa := c.RGBA()
						r += int(rr >> 8)
						g += int(gg >> 8)
						bl += int(bb >> 8)
						a += int(aa >> 8)
						n++
					}
				}
				rgba.SetRGBA(x, y, color.RGBA{
					R: uint8(r / n), G: uint8(g / n), B: uint8(bl / n), A: uint8(a / n),
				})
			}
		}
	}

	var buf bytes.Buffer
	if isJPG {
		err = jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&buf, rgba)
	}
	if err != nil {
		return data
	}
	return buf.Bytes()
}
