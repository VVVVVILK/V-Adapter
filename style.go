// style.go — 画风预设：为客户端（酒馆）发来的生图请求附加统一画风。
//
// 用户在管理面板「画风预设」页选中一个风格（或填自定义提示词）后，本服务在
// 转发上游前，把该画风的提示词拼到客户端 prompt 前面、把画风负向词合并进
// negative_prompt。全部预设均为动漫/插画向，并统一在负向词中排除真人照片感。
package main

import "strings"

// stylePreset 一个画风预设。
type stylePreset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	Prompt   string `json:"-"`
	Negative string `json:"-"`
}

// stylePresets 内置画风预设（ID 唯一）。Prompt 为附加到客户端 prompt 前的
// 英文画风描述；Negative 为合并进负向词的画风约束（含排除真人感）。
var stylePresets = map[string]stylePreset{
	"classic": {
		ID: "classic", Name: "经典赛璐璐", Desc: "昭和复古动画，干净线稿、纯色块平涂、色彩明快",
		Prompt:   "classic Japanese cel animation style, 1970s-80s retro anime, clean bold lineart, flat cel shading, vibrant colors, traditional anime aesthetic",
		Negative: "photorealistic, realistic photo, 3d render, live action, modern digipaint",
	},
	"ghibli": {
		ID: "ghibli", Name: "吉卜力治愈", Desc: "温暖治愈的手绘动画风，柔和水彩质感、细腻背景、怀旧氛围",
		Prompt:   "Studio Ghibli style, soft pastel colors, gentle hand-drawn watercolor texture, warm nostalgic atmosphere, detailed natural backgrounds, wholesome anime illustration",
		Negative: "photorealistic, realistic photo, 3d render, gritty, dark horror",
	},
	"dark": {
		ID: "dark", Name: "暗黑哥特", Desc: "黑红暗调、深影重对比、哥特暗黑奇幻氛围",
		Prompt:   "dark gothic anime style, deep shadows, moody atmosphere, black and crimson palette, dramatic chiaroscuro lighting, dark fantasy illustration, elegant macabre details",
		Negative: "photorealistic, realistic photo, bright cheerful colors, pastel, cute chibi",
	},
	"ink": {
		ID: "ink", Name: "黑白漫画", Desc: "黑白线稿漫画风，粗犷笔触、排线阴影、高对比",
		Prompt:   "black and white ink manga style, bold dynamic linework, hatching and crosshatching, high contrast, sketchy comic panel art, dramatic ink strokes",
		Negative: "color, photorealistic, 3d render, soft shading, painting",
	},
	"watercolor": {
		ID: "watercolor", Name: "清新水彩", Desc: "柔和水彩晕染、纸张纹理、梦幻淡色",
		Prompt:   "soft watercolor anime illustration, delicate washes and blooms, subtle color gradients, paper texture, dreamy light colors, gentle flowing composition",
		Negative: "photorealistic, thick oil paint, harsh lines, dark heavy shadows",
	},
	"pixel": {
		ID: "pixel", Name: "像素复古", Desc: "16-bit 像素游戏风，点阵清晰、有限色板、复古游戏感",
		Prompt:   "pixel art anime style, retro 16-bit game sprite, crisp pixels, limited color palette, chunky outlines, nostalgic video game aesthetic",
		Negative: "photorealistic, smooth gradient, high resolution painting, blurry",
	},
	"thick": {
		ID: "thick", Name: "日系厚涂", Desc: "现代插画厚涂，笔触明显、色彩饱满、细节丰富",
		Prompt:   "modern anime digital painting, thick paint brushwork, rich saturated colors, detailed rendering, light novel illustration style, polished character art",
		Negative: "photorealistic, flat cel shading, sketch, rough lineart",
	},
	"steam": {
		ID: "steam", Name: "蒸汽朋克", Desc: "黄铜齿轮、维多利亚服饰、复古机械风",
		Prompt:   "steampunk anime style, brass gears and cogs, victorian fashion, sepia and copper tones, intricate mechanical details, airship and clockwork atmosphere",
		Negative: "photorealistic, modern technology, neon cyberpunk, clean minimalism",
	},
	"cyber": {
		ID: "cyber", Name: "赛博朋克", Desc: "霓虹夜景、全息光效、雨夜都市科技感",
		Prompt:   "cyberpunk anime style, neon lights, futuristic cityscape, holographic glow, dark tech atmosphere, rain-soaked streets, reflective surfaces, high-tech dystopia",
		Negative: "photorealistic, rural scenery, historical period, soft pastel",
	},
	"cartoon": {
		ID: "cartoon", Name: "美式卡通", Desc: "粗描边、夸张表情、明亮平涂的欧美动画风",
		Prompt:   "american cartoon style, bold outlines, exaggerated expressive faces, bright flat colors, modern 2D animation, playful dynamic poses",
		Negative: "photorealistic, anime moe style, realistic proportions, muted colors",
	},
	"ukiyo": {
		ID: "ukiyo", Name: "浮世绘和风", Desc: "浮世绘版画风，流畅墨线、沉稳用色、传统日式韵味",
		Prompt:   "ukiyo-e woodblock print style, traditional Japanese art, flowing brush lines, muted ink colors, kimono and edo period aesthetic, wabi-sabi atmosphere",
		Negative: "photorealistic, western oil painting, neon colors, modern fashion",
	},
	"fantasy": {
		ID: "fantasy", Name: "西幻史诗", Desc: "魔幻史诗风，宏阔场景、魔法光效、华丽铠甲服饰",
		Prompt:   "epic dark fantasy anime style, sweeping landscapes, magical glowing effects, detailed armor and costumes, cinematic composition, mythical creatures, dramatic scale",
		Negative: "photorealistic, modern everyday scene, contemporary clothing, flat simple background",
	},
	"inkwash": {
		ID: "inkwash", Name: "水墨国风", Desc: "中国水墨画风，流动笔触、单色渐变、诗意留白",
		Prompt:   "Chinese ink wash painting style, flowing brushstrokes, monochrome ink gradients, poetic minimalism, traditional East Asian art, elegant negative space",
		Negative: "photorealistic, heavy color, western oil painting, cluttered composition",
	},
	"oil": {
		ID: "oil", Name: "古典油画", Desc: "古典油画质感，厚涂笔触、暖调光影、画布纹理",
		Prompt:   "classical oil painting style, rich textured brushstrokes, warm renaissance lighting, painterly canvas texture, baroque composition, timeless fine art feel",
		Negative: "photorealistic photo, flat vector, anime cel shading, digital gradient",
	},
}

// styleOrder 预设展示顺序（前端卡片按此排列）。
var styleOrder = []string{
	"classic", "ghibli", "dark", "ink", "watercolor", "pixel", "thick",
	"steam", "cyber", "cartoon", "ukiyo", "fantasy", "inkwash", "oil",
}

// normalizeStyleID 校验画风设置值：空/预设 id/custom 合法，未知 id 回落空（不启用）。
func normalizeStyleID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "custom" {
		return s
	}
	if _, ok := stylePresets[s]; ok {
		return s
	}
	return ""
}

// stylePresetList 面板展示用预设列表（名称 + 描述，不含内部 prompt）。
func stylePresetList() []map[string]string {
	list := make([]map[string]string, 0, len(styleOrder))
	for _, id := range styleOrder {
		p := stylePresets[id]
		list = append(list, map[string]string{
			"id": p.ID, "name": p.Name, "desc": p.Desc,
		})
	}
	return list
}

// applyStyle 把当前画风设置应用到客户端 prompt/neg（未启用或空时原样返回）。
func applyStyle(prompt, neg string) (string, string) {
	var pre, sneg string
	switch settingsStyle() {
	case "custom":
		pre = strings.TrimSpace(settingsStyleCustom())
	case "":
		return prompt, neg
	default:
		if p, ok := stylePresets[settingsStyle()]; ok {
			pre, sneg = p.Prompt, p.Negative
		}
	}
	if pre == "" {
		return prompt, neg
	}
	if prompt != "" {
		pre = pre + ", " + prompt
	}
	if sneg != "" {
		if neg == "" {
			neg = sneg
		} else {
			neg = sneg + ", " + neg
		}
	}
	return pre, neg
}
